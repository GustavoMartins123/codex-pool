package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/coder/websocket"
)

// Real HTTP/WS sockets, Passport credentials and provider adapters exercise
// the same external conversation ID through WS -> SSE -> Antigravity -> WS.
func TestAuthenticatedHandoffTransportAndProviderIsolation(t *testing.T) {
	for _, recovering := range []bool{false, true} {
		t.Run(fmt.Sprintf("recovery=%v", recovering), func(t *testing.T) {
			testAuthenticatedTransportIsolation(t, recovering)
		})
	}
}

func testAuthenticatedTransportIsolation(t *testing.T, recovering bool) {
	t.Setenv("POOL_JWT_SECRET", "test-secret")
	captured := make(chan string, 16)
	var recoveryMu sync.Mutex
	recoverySessions := map[string]string{}
	answerFor := func(body string) string {
		for step := 3; step >= 0; step-- {
			for _, owner := range []string{"alice", "bob"} {
				if strings.Contains(body, fmt.Sprintf("%s_private_question_%d", owner, step)) {
					return fmt.Sprintf("%s_private_answer_%d", owner, step)
				}
			}
		}
		return "missing question"
	}
	completed := func(answer string) []byte {
		data, _ := json.Marshal(map[string]any{"type": "response.completed", "response": map[string]any{
			"id": "resp_done", "status": "completed", "output": []any{map[string]any{"type": "message", "role": "assistant",
				"content": []any{map[string]any{"type": "output_text", "text": answer}}}}}})
		return data
	}
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if isWebSocketUpgradeRequest(r) {
			conn, err := websocket.Accept(w, r, &websocket.AcceptOptions{InsecureSkipVerify: true})
			if err != nil {
				t.Error(err)
				return
			}
			defer conn.CloseNow()
			ctx, cancel := context.WithTimeout(r.Context(), 5*time.Second)
			defer cancel()
			_, body, err := conn.Read(ctx)
			if err != nil {
				t.Error(err)
				return
			}
			captured <- string(body)
			if err := conn.Write(ctx, websocket.MessageText, completed(answerFor(string(body)))); err != nil {
				t.Error(err)
			}
			// The client closes after consuming the completed turn.
			_, _, _ = conn.Read(ctx)
			return
		}
		body, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
			return
		}
		captured <- string(body)
		answer := answerFor(string(body))
		w.Header().Set("Content-Type", "text/event-stream")
		if strings.Contains(string(body), "sessionId") {
			if recovering {
				var object map[string]any
				if err := json.Unmarshal(body, &object); err != nil {
					t.Error(err)
					return
				}
				request, _ := object["request"].(map[string]any)
				session := stringValue(request["sessionId"])
				recoveryMu.Lock()
				previous, seen := recoverySessions[answer]
				if !seen {
					recoverySessions[answer] = session
				}
				recoveryMu.Unlock()
				if !seen {
					w.Header().Set("Content-Type", "application/json")
					w.WriteHeader(http.StatusTooManyRequests)
					io.WriteString(w, `{"error":{"status":"RESOURCE_EXHAUSTED","message":"invalid session identifier"}}`)
					return
				}
				if session == "" || session == previous {
					t.Error("recovery reused stale session")
				}
			}
			data, _ := json.Marshal(map[string]any{"response": map[string]any{"candidates": []any{map[string]any{
				"content": map[string]any{"parts": []any{map[string]any{"text": answer}}}, "finishReason": "STOP"}}}})
			fmt.Fprintf(w, "data: %s\n\n", data)
		} else {
			data, _ := json.Marshal(map[string]any{"type": "content_block_delta", "index": 0,
				"delta": map[string]any{"type": "text_delta", "text": answer}})
			fmt.Fprintf(w, "event: content_block_delta\ndata: %s\n\nevent: message_stop\ndata: {\"type\":\"message_stop\"}\n\n", data)
		}
	}))
	defer upstream.Close()
	base, _ := url.Parse(upstream.URL)
	h := &proxyHandler{
		cfg: &config{maxAttempts: 1, maxInMemoryBodyBytes: 1 << 20, requestTimeout: 5 * time.Second,
			streamTimeout: 5 * time.Second, disableRefresh: true, websocketReadLimit: 8 << 20},
		pool: newPoolState([]*Account{
			{ID: "codex", Type: AccountTypeCodex, AccessToken: "codex", AccountID: "acct", PlanType: "pro"},
			{ID: "claude", Type: AccountTypeClaude, AccessToken: "claude", PlanType: "max"},
			{ID: "anti", Type: AccountTypeAntigravity, AccessToken: "anti", ProjectID: "project", PlanType: "pro"},
		}, false),
		registry:  NewProviderRegistry(NewCodexProviderWithRealtime(base, base, base, base), NewClaudeProvider(base), nil, NewAntigravityProvider(base, base)),
		transport: http.DefaultTransport, antigravityTransport: http.DefaultTransport,
		metrics: newMetrics(), recent: newRecentErrors(5),
	}
	proxy := httptest.NewServer(testPoolAuthenticatedHandler(t, h))
	defer proxy.Close()
	models := []string{"gpt-5.6-sol", "claude-opus-4-6", "antigravity/gemini-3.8-flash-high", "gpt-5.6-sol"}
	for step, model := range models {
		for _, owner := range []string{"alice", "bob"} {
			question := fmt.Sprintf("%s_private_question_%d", owner, step)
			body := map[string]any{"model": model, "conversation_id": "shared-transport-id", "input": question, "stream": true}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			if step == 0 || step == 3 {
				body["type"] = "response.create"
				conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(proxy.URL, "http")+"/v1/responses", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": {"Bearer " + generateClaudePoolToken("test-secret", owner)}}})
				if err != nil {
					cancel()
					t.Fatal(err)
				}
				data, _ := json.Marshal(body)
				if err = conn.Write(ctx, websocket.MessageText, data); err != nil {
					conn.CloseNow()
					cancel()
					t.Fatal(err)
				}
				_, response, err := conn.Read(ctx)
				conn.CloseNow()
				if err != nil || !strings.Contains(string(response), fmt.Sprintf("%s_private_answer_%d", owner, step)) {
					cancel()
					t.Fatalf("WS response: %s %v", response, err)
				}
			} else {
				path := "/v1/responses"
				if step == 1 {
					path = "/v1/messages"
					delete(body, "input")
					body["messages"] = []any{map[string]any{"role": "user", "content": question}}
					body["max_tokens"] = 128
				}
				data, _ := json.Marshal(body)
				r, _ := http.NewRequestWithContext(ctx, http.MethodPost, proxy.URL+path, strings.NewReader(string(data)))
				r.Header.Set("Content-Type", "application/json")
				r.Header.Set("Authorization", "Bearer "+generateClaudePoolToken("test-secret", owner))
				resp, err := http.DefaultClient.Do(r)
				if err != nil {
					cancel()
					t.Fatal(err)
				}
				response, err := io.ReadAll(resp.Body)
				resp.Body.Close()
				if err != nil || resp.StatusCode != 200 || !strings.Contains(string(response), fmt.Sprintf("%s_private_answer_%d", owner, step)) {
					cancel()
					t.Fatalf("HTTP owner=%s step=%d: %d %s %v", owner, step, resp.StatusCode, response, err)
				}
			}
			cancel()
			attempts := 1
			if recovering && step == 2 {
				attempts = 2
			}
			for attempt := 0; attempt < attempts; attempt++ {
				var sent string
				select {
				case sent = <-captured:
				case <-time.After(time.Second):
					t.Fatal("missing upstream capture")
				}
				other := "alice"
				if owner == other {
					other = "bob"
				}
				if strings.Contains(sent, other+"_private_") {
					t.Fatalf("cross-principal leak step=%d: %s", step, sent)
				}
				for prior := 0; prior < step; prior++ {
					for _, kind := range []string{"question", "answer"} {
						marker := fmt.Sprintf("%s_private_%s_%d", owner, kind, prior)
						if !strings.Contains(sent, marker) {
							t.Fatalf("continuity lost %s step=%d: %s", marker, step, sent)
						}
					}
				}
			}
			key := conversationScopedKey(testPoolIdentity(t, h, owner), "shared-transport-id")
			if !auditWaitFor(t, time.Second, func() bool {
				state, ok := h.getContextHandoff().State(key)
				encoded, _ := json.Marshal(state.Messages)
				return ok && strings.Contains(string(encoded), fmt.Sprintf("%s_private_answer_%d", owner, step))
			}) {
				t.Fatalf("assistant turn not recorded owner=%s step=%d", owner, step)
			}
		}
	}
}
