package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestAntigravityTransitionRecoveryRetriesOnce(t *testing.T) {
	for _, test := range []struct {
		name       string
		firstError string
		alwaysFail bool
		wantStatus int
	}{
		{"session recovered", `{"error":{"status":"RESOURCE_EXHAUSTED","message":"invalid session identifier"}}`, false, 200},
		{"context recovered", `{"error":{"status":"RESOURCE_EXHAUSTED","message":"context mismatch"}}`, false, 200},
		{"session final failure", `{"error":{"status":"RESOURCE_EXHAUSTED","message":"invalid session identifier"}}`, true, 429},
	} {
		t.Run(test.name, func(t *testing.T) {
			daily, _ := url.Parse("https://daily.example")
			prod, _ := url.Parse("https://prod.example")
			account := &Account{Type: AccountTypeAntigravity, ID: "anti", AccessToken: "token", ProjectID: "project", PlanType: "pro"}
			var sessions []string
			h := &proxyHandler{
				cfg:      &config{maxAttempts: 1, requestTimeout: time.Second, streamTimeout: time.Second},
				pool:     newPoolState([]*Account{account}, false),
				registry: NewProviderRegistry(nil, nil, nil, NewAntigravityProvider(daily, prod)),
				transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					body, _ := io.ReadAll(req.Body)
					var root map[string]any
					if err := json.Unmarshal(body, &root); err != nil {
						t.Fatal(err)
					}
					request, _ := root["request"].(map[string]any)
					sessions = append(sessions, stringValue(request["sessionId"]))
					if len(sessions) == 1 || test.alwaysFail {
						return &http.Response{StatusCode: 429, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(test.firstError)), Request: req}, nil
					}
					response := `data: {"response":{"candidates":[{"content":{"parts":[{"text":"recovered"}]},"finishReason":"STOP"}]}}` + "\n\n"
					return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(response)), Request: req}, nil
				}),
			}
			store := h.getContextHandoff()
			conversationID := "recovery-conversation"
			if _, _, err := store.Prepare(conversationID, AccountTypeCodex, "/v1/responses", contextTestBody(contextFormatResponses, "first", false)); err != nil {
				t.Fatal(err)
			}
			body := addContextOpaqueState(t, contextTestBody(contextFormatResponses, "second", false))
			clean, result, err := store.Prepare(conversationID, AccountTypeAntigravity, "/v1/responses", body)
			if err != nil || !result.Switched {
				t.Fatalf("handoff: %+v %v", result, err)
			}
			r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(clean)))
			w := httptest.NewRecorder()
			if !h.handleAntigravityProxy(w, r, clean, "antigravity/gemini-3.8-flash-high", conversationID, "user", "origin", "127.0.0.1", "req", "") {
				t.Fatal("request not handled")
			}
			if w.Code != test.wantStatus || len(sessions) != 2 || sessions[0] == sessions[1] {
				t.Fatalf("status=%d sessions=%v body=%s", w.Code, sessions, w.Body.String())
			}
			if test.alwaysFail && !strings.Contains(w.Body.String(), "invalid session") {
				t.Fatalf("final error was masked: %s", w.Body.String())
			}
			state, _ := store.State(conversationID)
			if state.TransitionEpoch != 2 || state.ProviderSessions[AccountTypeAntigravity].Established == test.alwaysFail {
				t.Fatalf("recovery state=%+v", state.ProviderSessions[AccountTypeAntigravity])
			}
			account.mu.Lock()
			cooldowns := len(account.ModelRateLimits)
			account.mu.Unlock()
			if cooldowns != 0 {
				t.Fatalf("session/context failure changed quota cooldowns: %d", cooldowns)
			}
		})
	}
}
