package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"
)

// The pool offers Antigravity capacity to test that unavailable Codex requests stay on Codex.
func newAntigravityOnlyHandler(t *testing.T, transport http.RoundTripper) *proxyHandler {
	t.Helper()
	t.Setenv("POOL_JWT_SECRET", "test-secret")

	codexBase, _ := url.Parse("https://chatgpt.com/backend-api/codex")
	claudeBase, _ := url.Parse("https://api.anthropic.com")
	geminiBase, _ := url.Parse("https://generativelanguage.googleapis.com")
	antigravityDaily, _ := url.Parse("https://daily.example")
	antigravityProd, _ := url.Parse("https://prod.example")

	antiAcc := &Account{Type: AccountTypeAntigravity, ID: "anti-1", AccessToken: "anti-token", google: googleAccountState{ProjectID: "proj-1"}, PlanType: "pro"}
	pool := newPoolState([]*Account{antiAcc}, false)

	return &proxyHandler{
		cfg: &config{
			requestTimeout:       5 * time.Second,
			streamTimeout:        5 * time.Second,
			maxInMemoryBodyBytes: 4 << 20,
			maxAttempts:          1,
		},
		transport: transport,
		pool:      pool,
		registry: NewProviderRegistry(
			NewCodexProvider(codexBase, codexBase, nil),
			NewClaudeProvider(claudeBase),
			NewGeminiProvider(geminiBase, geminiBase),
			NewAntigravityProvider(antigravityDaily, antigravityProd),
		),
		metrics: newMetrics(),
		recent:  newRecentErrors(5),
	}
}

func postCodexModelRequest(t *testing.T, h *proxyHandler, model string) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"model":"` + model + `","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}],"stream":true}`
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+generateClaudePoolToken("test-secret", "user-1"))
	req.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	testPoolServeHTTP(t, h, w, req)
	return w
}

func TestUnavailableCodexModelDoesNotCallOtherProviders(t *testing.T) {
	h := newAntigravityOnlyHandler(t, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Errorf("unavailable Codex model reached an upstream: %s", req.URL.Host)
		return &http.Response{StatusCode: http.StatusInternalServerError, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
	}))

	w := postCodexModelRequest(t, h, "gpt-5.6-sol")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body=%q)", w.Code, w.Body.String())
	}
	if strings.TrimSpace(w.Body.String()) == "" {
		t.Fatal("unavailable model returned an empty response")
	}
}
