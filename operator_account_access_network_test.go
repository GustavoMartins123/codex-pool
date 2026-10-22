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

func TestOperatorAccountAccessHTTPAndPersistentWebSocketRevocation(t *testing.T) {
	t.Setenv("POOL_JWT_SECRET", "test-secret")
	var httpCalls, frames atomic.Int32
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isWebSocketUpgradeRequest(r) {
			conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
			if err != nil {
				return
			}
			defer conn.CloseNow()
			for {
				_, data, err := conn.Read(r.Context())
				if err != nil {
					return
				}
				if isCodexResponseCreate(data) {
					frames.Add(1)
					_ = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"operator-test","usage":{"input_tokens":2,"output_tokens":1,"total_tokens":3}}}`))
				}
			}
		}
		httpCalls.Add(1)
		w.Header().Set("Content-Type", "text/event-stream")
		io.WriteString(w, "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"operator-http\",\"usage\":{\"input_tokens\":2,\"output_tokens\":1,\"total_tokens\":3}}}\n\n")
	}))
	defer upstream.Close()
	base, _ := url.Parse(upstream.URL)
	a := &Account{ID: "private-operator-network", Type: AccountTypeCodex, PlanType: "pro", CyberAccess: true, AccessToken: "private-upstream"}
	fx := newCodexProxyFixture(t, base, []*Account{a})
	h := fx.handler
	h.cfg.maxAttempts = 1
	h.aliases = newModelAliases(nil)
	addTestPassportOperator(t, h.passport, "alice")
	addTestPassportOperator(t, h.passport, "bob")
	if _, err := h.passport.setPrincipalKind("bob", "alice", PrincipalMember); err != nil {
		t.Fatal(err)
	}
	if err := h.passport.initializeAccountAuthority(nil); err != nil {
		t.Fatal(err)
	}
	if err := h.passport.db.Update(func(tx *bbolt.Tx) error {
		return putJSON(tx.Bucket([]byte(bucketAccountResources)), resourceKey(a.Type, a.ID), accountResource{Version: 2, ID: a.ID, Provider: a.Type, OwnerID: "alice", AddedBy: "alice", Status: "active", Revision: 1})
	}); err != nil {
		t.Fatal(err)
	}
	h.pool.accountAuthority = h.passport
	request := func() int {
		r, _ := http.NewRequest("POST", fx.server.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"hello"}`))
		r.Header.Set("Authorization", "Bearer "+generateClaudePoolToken("test-secret", "bob"))
		r.Header.Set("Content-Type", "application/json")
		resp, err := http.DefaultClient.Do(r)
		if err != nil {
			t.Fatal(err)
		}
		io.Copy(io.Discard, resp.Body)
		resp.Body.Close()
		return resp.StatusCode
	}
	if status := request(); status != 403 || httpCalls.Load() != 0 {
		t.Fatalf("default access: %d upstream=%d", status, httpCalls.Load())
	}
	if err := h.passport.setOperatorAccountAccess("bob", a.Type, a.ID, 1, true, "Support"); err != nil {
		t.Fatal(err)
	}
	if status := request(); status != 200 {
		t.Fatalf("activated HTTP: %d", status)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(fx.server.URL, "http")+"/responses", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + generateClaudePoolToken("test-secret", "bob")}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	frame := []byte(`{"type":"response.create","model":"gpt-5.5","input":[]}`)
	if err := conn.Write(ctx, websocket.MessageText, frame); err != nil {
		t.Fatal(err)
	}
	if _, _, err := conn.Read(ctx); err != nil {
		t.Fatal(err)
	}
	if err := h.passport.setOperatorAccountAccess("bob", a.Type, a.ID, 2, false, "Completed"); err != nil {
		t.Fatal(err)
	}
	_ = conn.Write(ctx, websocket.MessageText, frame)
	if _, _, err := conn.Read(ctx); err == nil {
		t.Fatal("deactivated WebSocket forwarded another turn")
	}
	if status := request(); status != 403 {
		t.Fatalf("deactivated HTTP: %d", status)
	}
	if httpCalls.Load() != 1 || frames.Load() != 1 {
		t.Fatalf("denied work reached provider: HTTP=%d WS=%d", httpCalls.Load(), frames.Load())
	}
}
