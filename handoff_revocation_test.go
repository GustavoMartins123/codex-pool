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
)

func TestAuthenticatedWebSocketRevocationStopsNextTurn(t *testing.T) {
	for _, mode := range []string{"client_revoked", "principal_suspended"} {
		t.Run(mode, func(t *testing.T) {
			t.Setenv("POOL_JWT_SECRET", "test-secret")
			var calls atomic.Int32
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if !isWebSocketUpgradeRequest(r) {
					calls.Add(1)
					io.WriteString(w, `{"output":[]}`)
					return
				}
				conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
				if err != nil {
					return
				}
				defer conn.CloseNow()
				for {
					_, _, err = conn.Read(r.Context())
					if err != nil {
						return
					}
					calls.Add(1)
					if err = conn.Write(r.Context(), websocket.MessageText, []byte(`{"type":"response.completed","response":{"id":"done","output":[]}}`)); err != nil {
						return
					}
				}
			}))
			defer upstream.Close()
			base, _ := url.Parse(upstream.URL)
			h := &proxyHandler{cfg: &config{maxAttempts: 1, maxInMemoryBodyBytes: 1 << 20, disableRefresh: true}, pool: newPoolState([]*Account{{ID: "codex", Type: AccountTypeCodex, AccessToken: "token", AccountID: "acct", PlanType: "pro"}}, false), registry: NewProviderRegistry(NewCodexProviderWithRealtime(base, base, base, base), nil, nil), transport: http.DefaultTransport, metrics: newMetrics(), recent: newRecentErrors(5)}
			proxy := httptest.NewServer(testPoolAuthenticatedHandler(t, h))
			defer proxy.Close()
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			dial := func(owner string) *websocket.Conn {
				t.Helper()
				conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(proxy.URL, "http")+"/v1/responses", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + generateClaudePoolToken("test-secret", owner)}}})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { conn.CloseNow() })
				return conn
			}
			turn := func(conn *websocket.Conn) {
				t.Helper()
				if err := conn.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"gpt-5.6-sol","conversation_id":"shared-revocation-id","input":"private turn"}`)); err != nil {
					t.Fatal(err)
				}
				if _, _, err := conn.Read(ctx); err != nil {
					t.Fatal(err)
				}
			}
			alice, bob := dial("alice"), dial("bob")
			turn(alice)
			turn(bob)
			if mode == "client_revoked" {
				identity := testPoolIdentity(t, h, "alice")
				principal, client := splitClientIdentity(identity)
				if err := h.passport.revokeClient("operator", principal, client); err != nil {
					t.Fatal(err)
				}
			} else {
				if _, err := h.passport.setPrincipalStatus("operator", "alice", PrincipalSuspended); err != nil {
					t.Fatal(err)
				}
			}
			before := calls.Load()
			if err := alice.Write(ctx, websocket.MessageText, []byte(`{"type":"response.create","model":"gpt-5.6-sol","input":"after revocation"}`)); err != nil {
				t.Fatal(err)
			}
			if _, data, err := alice.Read(ctx); err == nil {
				t.Fatalf("revoked connection received a new response: %s", data)
			}
			if calls.Load() != before {
				t.Fatal("revoked frame reached upstream")
			}
			r, _ := http.NewRequestWithContext(ctx, http.MethodPost, proxy.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5.6-sol","input":"after revocation"}`))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Authorization", "Bearer "+generateClaudePoolToken("test-secret", "alice"))
			resp, err := http.DefaultClient.Do(r)
			if err != nil {
				t.Fatal(err)
			}
			resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden || calls.Load() != before {
				t.Fatalf("revoked HTTP request: %d", resp.StatusCode)
			}
			conn, resp, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(proxy.URL, "http")+"/v1/responses", &websocket.DialOptions{HTTPHeader: r.Header})
			if conn != nil {
				conn.CloseNow()
			}
			if resp != nil {
				defer resp.Body.Close()
			}
			if err == nil || resp == nil || resp.StatusCode != http.StatusForbidden {
				t.Fatalf("revoked WS handshake accepted: %v %v", resp, err)
			}
			turn(bob)
			if calls.Load() != before+1 {
				t.Fatal("unrelated principal was affected by revocation")
			}
		})
	}
}
