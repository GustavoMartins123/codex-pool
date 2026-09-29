package main

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// Exercise authentication, routing, upstream payloads and assistant recording
// together, rather than supplying an owner directly to the handoff store.
func TestAuthenticatedHTTPHandoffPrincipalIsolation(t *testing.T) {
	t.Setenv("POOL_JWT_SECRET", "test-secret")
	codexBase, _ := url.Parse("https://codex.mock")
	claudeBase, _ := url.Parse("https://claude.mock")
	var captured, capturedHost, answer string
	h := &proxyHandler{
		cfg: &config{maxAttempts: 1, maxInMemoryBodyBytes: 1 << 20,
			requestTimeout: 5 * time.Second, streamTimeout: 5 * time.Second},
		pool: newPoolState([]*Account{
			{ID: "codex-a", Type: AccountTypeCodex, AccessToken: "codex-token", AccountID: "codex-account", PlanType: "pro"},
			{ID: "claude-a", Type: AccountTypeClaude, AccessToken: "claude-token", PlanType: "max"},
		}, false),
		registry: NewProviderRegistry(NewCodexProvider(codexBase, codexBase, nil), NewClaudeProvider(claudeBase), nil),
		metrics:  newMetrics(),
		recent:   newRecentErrors(5),
		transport: roundTripFunc(func(r *http.Request) (*http.Response, error) {
			body, err := io.ReadAll(r.Body)
			if err != nil {
				return nil, err
			}
			captured = string(body)
			capturedHost = r.URL.Host
			var response any
			if r.URL.Host == "claude.mock" {
				response = map[string]any{"id": "msg_test", "type": "message", "role": "assistant",
					"content": []any{map[string]any{"type": "text", "text": answer}}, "stop_reason": "end_turn"}
			} else {
				response = map[string]any{"id": "resp_test", "object": "response", "status": "completed",
					"output": []any{map[string]any{"type": "message", "role": "assistant",
						"content": []any{map[string]any{"type": "output_text", "text": answer}}}}}
			}
			encoded, err := json.Marshal(response)
			if err != nil {
				return nil, err
			}
			return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}},
				Body: io.NopCloser(strings.NewReader(string(encoded))), Request: r}, nil
		}),
	}
	models := []string{"gpt-5.6-sol", "claude-opus-4-6", "gpt-5.6-sol"}
	for step, model := range models {
		for _, owner := range []string{"alice", "bob"} {
			other := "alice"
			if owner == other {
				other = "bob"
			}
			question := fmt.Sprintf("%s_private_question_%d", owner, step)
			answer = fmt.Sprintf("%s_private_answer_%d", owner, step)
			body, err := json.Marshal(map[string]any{"model": model, "conversation_id": "shared-external-id",
				"input": []any{map[string]any{"type": "message", "role": "user", "content": []any{
					map[string]any{"type": "input_text", "text": question}}}}, "stream": false})
			if err != nil {
				t.Fatal(err)
			}
			path, expectedHost := "/v1/responses", "codex.mock"
			if step == 1 {
				path, expectedHost = "/v1/messages", "claude.mock"
				body, err = json.Marshal(map[string]any{"model": model, "conversation_id": "shared-external-id",
					"messages": []any{map[string]any{"role": "user", "content": question}}, "max_tokens": 128, "stream": false})
				if err != nil {
					t.Fatal(err)
				}
			}
			r := httptest.NewRequest(http.MethodPost, path, strings.NewReader(string(body)))
			r.Header.Set("Content-Type", "application/json")
			r.Header.Set("Authorization", "Bearer "+generateClaudePoolToken("test-secret", owner))
			w := httptest.NewRecorder()
			captured = ""
			testPoolServeHTTP(t, h, w, r)
			if w.Code != http.StatusOK {
				t.Fatalf("owner=%s step=%d status=%d body=%s", owner, step, w.Code, w.Body.String())
			}
			if !strings.Contains(captured, question) {
				t.Fatalf("current question missing: %s", captured)
			}
			if capturedHost != expectedHost {
				t.Fatalf("provider switch fixture failed: got %s want %s", capturedHost, expectedHost)
			}
			if strings.Contains(captured, other+"_private_") {
				t.Fatalf("cross-principal upstream leak: %s", captured)
			}
			for prior := 0; prior < step; prior++ {
				for _, marker := range []string{fmt.Sprintf("%s_private_question_%d", owner, prior), fmt.Sprintf("%s_private_answer_%d", owner, prior)} {
					if !strings.Contains(captured, marker) {
						t.Fatalf("same-owner continuity lost %q: %s", marker, captured)
					}
				}
			}
		}
	}
}
