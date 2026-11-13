package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
)

func qwenTestHandler(t *testing.T, transport http.RoundTripper, accounts ...*Account) *proxyHandler {
	t.Helper()
	base := mustParse("https://qwen.example/compatible-mode/v1")
	other := mustParse("https://other.example")
	return &proxyHandler{
		cfg:       &config{poolDir: t.TempDir(), qwenBase: base, qwenPlanBase: mustParse(defaultQwenPlanBase), maxAttempts: 1, maxInMemoryBodyBytes: 1 << 20},
		pool:      newPoolState(accounts, false),
		registry:  NewProviderRegistry(NewCodexProvider(other, other, nil), NewClaudeProvider(other), NewGeminiProvider(other, other), NewQwenProvider(base), NewOpencodeGoProvider(other)),
		transport: transport, metrics: newMetrics(), recent: newRecentErrors(5), aliases: newModelAliases(nil),
	}
}

func qwenJSONResponse(status int, body string) *http.Response {
	return &http.Response{StatusCode: status, Status: fmt.Sprintf("%d %s", status, http.StatusText(status)), Header: http.Header{"Content-Type": {"application/json"}}, Body: io.NopCloser(strings.NewReader(body))}
}

func TestQwenProxyRoutesClientProtocolsAndUsage(t *testing.T) {
	for _, test := range []struct {
		name, path, body, upstream string
		stream                     bool
	}{
		{"chat", "/v1/chat/completions", `{"model":"QWEN3.8-MAX","messages":[{"role":"user","content":"hi"}]}`, "/chat/completions", false},
		{"responses", "/v1/responses", `{"model":"qwen3.8-max","input":"hi"}`, "/chat/completions", false},
		{"codex path", "/backend-api/codex/responses", `{"model":"qwen3.8-max","input":"hi"}`, "/chat/completions", false},
		{"anthropic", "/v1/messages", `{"model":"qwen3.8-max","max_tokens":32,"messages":[{"role":"user","content":"hi"}]}`, "/chat/completions", false},
		{"streaming", "/v1/chat/completions", `{"model":"qwen3.8-max","stream":true,"messages":[{"role":"user","content":"hi"}]}`, "/chat/completions", true},
	} {
		t.Run(test.name, func(t *testing.T) {
			t.Setenv("POOL_JWT_SECRET", "qwen-routing-test-secret")
			account := &Account{ID: "qwen", Type: AccountTypeQwen, AccessToken: "sk-sp-qwen-upstream-test", PlanType: "token_plan", Models: map[string]DiscoveredModel{"qwen3.8-max": {ID: "qwen3.8-max", Protocol: "openai"}}}
			called := false
			h := qwenTestHandler(t, roundTripFunc(func(req *http.Request) (*http.Response, error) {
				called = true
				if req.URL.String() != "https://qwen.example/compatible-mode/v1"+test.upstream || req.Header.Get("Authorization") != "Bearer sk-sp-qwen-upstream-test" {
					t.Fatalf("wrong Qwen upstream: %s", req.URL)
				}
				var payload map[string]any
				if err := json.NewDecoder(req.Body).Decode(&payload); err != nil || payload["model"] != "qwen3.8-max" {
					t.Fatalf("invalid upstream body: %v %v", payload, err)
				}
				if test.upstream == "/chat/completions" && payload["messages"] == nil {
					t.Fatal("chat messages missing after translation")
				}
				if test.stream {
					options, _ := payload["stream_options"].(map[string]any)
					if options["include_usage"] != true {
						t.Fatal("Qwen streaming request did not request token accounting")
					}
					response := qwenJSONResponse(200, "data: {\"id\":\"chat-test\",\"model\":\"qwen3.8-max\",\"choices\":[{\"index\":0,\"delta\":{\"content\":\"hello\"}}]}\n\ndata: {\"usage\":{\"prompt_tokens\":100,\"completion_tokens\":20,\"prompt_tokens_details\":{\"cached_tokens\":30}}}\n\ndata: [DONE]\n\n")
					response.Header.Set("Content-Type", "text/event-stream")
					return response, nil
				}
				return qwenJSONResponse(200, `{"id":"chat-test","model":"qwen3.8-max","choices":[{"index":0,"message":{"role":"assistant","content":"hello"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":30}}}`), nil
			}), account)
			req := httptest.NewRequest("POST", test.path, strings.NewReader(test.body))
			req.Header.Set("Content-Type", "application/json")
			req.Header.Set("X-Api-Key", generateClaudePoolToken("qwen-routing-test-secret", "qwen-client"))
			rr := httptest.NewRecorder()
			testPoolProxyRequest(t, h, rr, req, "qwen-proxy")
			if rr.Code != 200 || !called || !strings.Contains(rr.Body.String(), "hello") {
				t.Fatalf("proxy failed: status=%d called=%v body=%s", rr.Code, called, rr.Body.String())
			}
		})
	}
}

func TestQwenDiscoveryFailureClearsCatalogAndRecovers(t *testing.T) {
	path := filepath.Join(t.TempDir(), "qwen.json")
	if err := writeAccountFile(path, []byte(`{"api_key":"sk-sp-discovery-test-key"}`)); err != nil {
		t.Fatal(err)
	}
	account := &Account{ID: "qwen", Type: AccountTypeQwen, File: path, AccessToken: "sk-sp-discovery-test-key", Models: map[string]DiscoveredModel{"qwen-old": {ID: "qwen-old"}}}
	h := qwenTestHandler(t, nil, account)
	failed := roundTripFunc(func(*http.Request) (*http.Response, error) { return qwenJSONResponse(503, `{}`), nil })
	if syncProviderModels(context.Background(), failed, h.registry, account) == nil || len(account.Models) != 0 || account.HealthError == "" {
		t.Fatal("failed discovery kept a usable Qwen catalog")
	}
	if h.pool.candidateForModel("", nil, AccountTypeQwen, "", "", "qwen-old") != nil {
		t.Fatal("an undiscovered Qwen model remained routable")
	}
	good := roundTripFunc(func(*http.Request) (*http.Response, error) {
		return qwenJSONResponse(200, `{"data":[{"id":"qwen-new"}]}`), nil
	})
	if err := syncProviderModels(context.Background(), good, h.registry, account); err != nil || account.HealthError != "" || len(account.Models) != 1 {
		t.Fatalf("Qwen discovery did not recover: %v", err)
	}
	if isOpencodeGoModel("qwen3.8-max") || !isOpencodeGoModel("opencode-go/qwen3.8-max") {
		t.Fatal("Qwen routing confused the native and namespaced Go providers")
	}
}
