package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestChatMessagesProxyRejectsWrongUpstreamProtocol(t *testing.T) {
	t.Setenv("POOL_JWT_SECRET", "chat-messages-proxy-test-secret")
	cases := []struct {
		name, path, request, upstreamPath, upstreamBody string
		account                                         *Account
	}{
		{
			name: "messages client", path: "/v1/messages", upstreamPath: "/chat/completions",
			request:      `{"model":"qwen3.8-max","max_tokens":32,"messages":[{"role":"user","content":"hello"}]}`,
			upstreamBody: `{"object":"response","output":[],"unexpected":"upstream-body-must-not-be-relayed"}`,
			account: &Account{ID: "qwen", Type: AccountTypeQwen, AccessToken: "upstream-test", PlanType: "token_plan",
				Models: map[string]DiscoveredModel{"qwen3.8-max": {ID: "qwen3.8-max", Protocol: "openai"}}},
		},
		{
			name: "chat client", path: "/v1/chat/completions", upstreamPath: "/messages",
			request:      `{"model":"claude-sonnet-5-5","messages":[{"role":"user","content":"hello"}]}`,
			upstreamBody: `{"object":"chat.completion","choices":[],"unexpected":"upstream-body-must-not-be-relayed"}`,
			account:      &Account{ID: "claude", Type: AccountTypeClaude, AccessToken: "upstream-test", PlanType: "pro"},
		},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			called := false
			h := qwenTestHandler(t, roundTripFunc(func(req *http.Request) (*http.Response, error) {
				called = true
				if !strings.HasSuffix(req.URL.Path, tc.upstreamPath) {
					t.Fatalf("upstream path = %s, want suffix %s", req.URL.Path, tc.upstreamPath)
				}
				return qwenJSONResponse(http.StatusOK, tc.upstreamBody), nil
			}), tc.account)
			request := httptest.NewRequest(http.MethodPost, tc.path, strings.NewReader(tc.request))
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("X-Api-Key", generateClaudePoolToken("chat-messages-proxy-test-secret", "translation-client"))
			recorder := httptest.NewRecorder()
			testPoolProxyRequest(t, h, recorder, request, "chat-messages-test")
			if !called || recorder.Code != http.StatusBadGateway ||
				!strings.Contains(recorder.Body.String(), "invalid upstream response") ||
				strings.Contains(recorder.Body.String(), "upstream-body-must-not-be-relayed") {
				t.Fatalf("called=%v status=%d body=%s", called, recorder.Code, recorder.Body)
			}
		})
	}
}
