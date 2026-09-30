package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/coder/websocket"
	"go.etcd.io/bbolt"
)

func TestPrivateAccountAuthenticatedStreamAndWebSocketWithdrawal(t *testing.T) {
	t.Setenv("POOL_JWT_SECRET", "test-secret")
	var frames, requests atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("Authorization") != "Bearer upstream-private" {
			t.Error("wrong upstream credential")
		}
		if isWebSocketUpgradeRequest(r) {
			conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
			if err != nil {
				return
			}
			defer conn.CloseNow()
			ctx, cancel := context.WithTimeout(r.Context(), 8*time.Second)
			defer cancel()
			for {
				_, _, err := conn.Read(ctx)
				if err != nil {
					return
				}
				frames.Add(1)
				if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"response-private","status":"completed","output":[]}}`)); err != nil {
					return
				}
			}
		}
		requests.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"response-private\",\"status\":\"completed\",\"output\":[]}}\n\n")
	}))
	defer upstream.Close()
	base, _ := url.Parse(upstream.URL)
	account := &Account{ID: "private", Type: AccountTypeCodex, AccessToken: "upstream-private", AccountID: "upstream-account", PlanType: "pro"}
	fx := newCodexProxyFixture(t, base, []*Account{account})
	h := fx.handler
	h.cfg.maxAttempts = 1
	h.aliases = newModelAliases(nil)
	addTestPassportOperator(t, h.passport, "alice")
	addTestPassportOperator(t, h.passport, "bob")
	if err := h.passport.initializeAccountAuthority(nil); err != nil {
		t.Fatal(err)
	}
	if err := h.passport.db.Update(func(tx *bbolt.Tx) error {
		return putJSON(tx.Bucket([]byte(bucketAccountResources)), resourceKey(account.Type, account.ID), accountResource{Version: 2, ID: account.ID, Provider: account.Type, OwnerID: "alice", AddedBy: "alice", Status: "active", Revision: 2})
	}); err != nil {
		t.Fatal(err)
	}
	h.pool.accountAuthority = h.passport
	stream := func(actor string) int {
		r, _ := http.NewRequest("POST", fx.server.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"hello"}`))
		r.Header.Set("Authorization", "Bearer "+generateClaudePoolToken("test-secret", actor))
		r.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		defer resp.Body.Close()
		body, _ := io.ReadAll(resp.Body)
		if actor == "alice" && resp.StatusCode == 200 && !strings.Contains(string(body), "response.completed") {
			t.Fatal("stream lost completion")
		}
		return resp.StatusCode
	}
	if code := stream("alice"); code != 200 {
		t.Fatalf("owner stream: %d", code)
	}
	if code := stream("bob"); code < 400 {
		t.Fatalf("foreign stream: %d", code)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	wsURL := "ws" + strings.TrimPrefix(fx.server.URL, "http") + "/responses"
	foreign, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + generateClaudePoolToken("test-secret", "bob")}}})
	if err == nil {
		foreign.CloseNow()
		t.Fatal("foreign websocket admitted")
	}
	conn, _, err := websocket.Dial(ctx, wsURL, &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + generateClaudePoolToken("test-secret", "alice")}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	payload := []byte(`{"type":"response.create","response":{"model":"gpt-5.5","input":[]}}`)
	if err := conn.Write(ctx, websocket.MessageText, payload); err != nil {
		t.Fatal(err)
	}
	if _, data, err := conn.Read(ctx); err != nil || !strings.Contains(string(data), "response.completed") {
		t.Fatalf("owner websocket: %s %v", data, err)
	}
	if err := h.passport.withdrawAccount("alice", account.Type, account.ID, 2); err != nil {
		t.Fatal(err)
	}
	if err := conn.Write(ctx, websocket.MessageText, payload); err == nil {
		_, data, err := conn.Read(ctx)
		if err == nil && !strings.Contains(string(data), "account_access_denied") {
			t.Fatalf("withdrawn websocket still active: %s", data)
		}
	}
	if code := stream("alice"); code < 400 {
		t.Fatalf("withdrawn stream: %d", code)
	}
	if frames.Load() != 1 || requests.Load() != 1 {
		t.Fatalf("unauthorized upstream traffic: frames=%d requests=%d", frames.Load(), requests.Load())
	}
}
