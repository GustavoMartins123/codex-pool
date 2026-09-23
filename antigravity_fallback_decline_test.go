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

// newFallbackDeclineHandler builds a proxyHandler whose pool has no codex
// accounts and a single live antigravity account, so a request for a codex
// model immediately enters the fallback branch of the attempt loop.
func newFallbackDeclineHandler(t *testing.T, transport http.RoundTripper) *proxyHandler {
	t.Helper()
	t.Setenv("POOL_JWT_SECRET", "test-secret")

	codexBase, _ := url.Parse("https://chatgpt.com/backend-api/codex")
	claudeBase, _ := url.Parse("https://api.anthropic.com")
	geminiBase, _ := url.Parse("https://generativelanguage.googleapis.com")
	antigravityDaily, _ := url.Parse("https://daily.example")
	antigravityProd, _ := url.Parse("https://prod.example")

	antiAcc := &Account{Type: AccountTypeAntigravity, ID: "anti-1", AccessToken: "anti-token", ProjectID: "proj-1", PlanType: "pro"}
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
	h.ServeHTTP(w, req)
	return w
}

// A fallback candidate that carries "gemini" resolves to the antigravity
// provider via lookupModelMetadata but is not routable by
// shouldRouteAntigravityModel. The loop must answer with a real 503 so the
// client can back off — never an empty 200.
func TestFallbackToNonRoutableAntigravityModelWrites503(t *testing.T) {
	h := newFallbackDeclineHandler(t, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		t.Errorf("upstream must not be called for a non-routable fallback: %s", req.URL.Host)
		return &http.Response{StatusCode: http.StatusInternalServerError, Body: io.NopCloser(strings.NewReader("")), Request: req}, nil
	}))
	h.pool.fallbackGraph.SetRoute("gpt-5.6-sol", FallbackRule{
		OnUnavailable: []string{"gemini-custom-unknown"},
	})

	w := postCodexModelRequest(t, h, "gpt-5.6-sol")
	if w.Code != http.StatusServiceUnavailable {
		t.Fatalf("status = %d, want 503 (body=%q)", w.Code, w.Body.String())
	}
	if strings.TrimSpace(w.Body.String()) == "" {
		t.Fatal("declined fallback must write an error body, got empty response")
	}
}

// A routable antigravity fallback is served normally through the same branch.
func TestFallbackToRoutableAntigravityModelServesResponse(t *testing.T) {
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if !strings.Contains(req.URL.Host, "example") {
			return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("not found")), Request: req}, nil
		}
		respSSE := `data: {"response":{"responseId":"anti-resp-1","candidates":[{"content":{"parts":[{"text":"hello from antigravity"}]},"finishReason":"STOP"}]}}` + "\n\n"
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(respSSE)),
			Request:    req,
		}, nil
	})
	h := newFallbackDeclineHandler(t, transport)
	h.pool.fallbackGraph.SetRoute("gpt-5.6-sol", FallbackRule{
		OnUnavailable: []string{"antigravity/gemini-3.8-flash-high"},
	})

	w := postCodexModelRequest(t, h, "gpt-5.6-sol")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "hello from antigravity") {
		var probe map[string]any
		_ = json.Unmarshal(w.Body.Bytes(), &probe)
		t.Fatalf("antigravity fallback response missing payload: %q", w.Body.String())
	}
}
