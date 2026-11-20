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

type mockUpstreamTransport struct {
	handler http.Handler
}

func (m *mockUpstreamTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	rec := httptest.NewRecorder()
	m.handler.ServeHTTP(rec, req)
	return rec.Result(), nil
}

func TestPoolAutoResponsesEndpointIntegration(t *testing.T) {
	t.Setenv("POOL_JWT_SECRET", "integration-secret")

	upstreamHit := make(chan string, 10)
	upstreamHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		var obj map[string]any
		_ = json.Unmarshal(body, &obj)
		m, _ := obj["model"].(string)
		upstreamHit <- m

		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"id\":\"resp_auto\",\"usage\":{\"input_tokens\":10,\"output_tokens\":20}}}\n\n"))
	})

	baseURL, _ := url.Parse("http://mock-upstream.local")
	codexAccount := &Account{
		Type:        AccountTypeCodex,
		ID:          "codex_auto_1",
		AccessToken: "test-token",
		PlanType:    "pro",
	}

	pool := newPoolState([]*Account{codexAccount}, false)
	handler := &proxyHandler{
		cfg:             &config{maxInMemoryBodyBytes: 1 << 20, maxSpoolBodyBytes: 1 << 20, maxAttempts: 1, streamTimeout: 5 * time.Second},
		transport:       &mockUpstreamTransport{handler: upstreamHandler},
		pool:            pool,
		registry:        NewProviderRegistry(NewCodexProvider(baseURL, baseURL, baseURL), NewClaudeProvider(baseURL), NewGeminiProvider(baseURL, baseURL)),
		metrics:         newMetrics(),
		recent:          newRecentErrors(50),
		routeTraces:     newRouteTraceStore(100),
		poolAuto:        newPoolAutoOrchestrator(nil),
		pricing:         testPricingData(t),
	}

	// Test 1: POST /v1/responses with pool/auto
	reqBody := `{"model":"pool/auto","stream":true,"input":"Hello pool auto"}`
	req := httptest.NewRequest("POST", "/v1/responses", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+generateClaudePoolToken("integration-secret", "user-auto"))
	rec := httptest.NewRecorder()

	testPoolServeHTTP(t, handler, rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK, got %d: %s", rec.Code, rec.Body.String())
	}

	// Verify upstream was hit with a resolved model instead of "pool/auto"
	select {
	case routedModel := <-upstreamHit:
		if routedModel == "pool/auto" || routedModel == "" {
			t.Fatalf("expected upstream to receive resolved model, got %q", routedModel)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("timed out waiting for upstream hit")
	}

	// Verify route trace was recorded
	reqID := rec.Header().Get("X-Pool-Request-Id")
	if reqID == "" {
		t.Fatalf("expected X-Pool-Request-Id header")
	}
	trace, ok := handler.routeTraces.Get(reqID)
	if !ok {
		t.Fatalf("expected trace for %s", reqID)
	}
	if !strings.HasPrefix(trace.Policy, "pool_auto:") {
		t.Errorf("expected trace policy to start with pool_auto:, got %s", trace.Policy)
	}
}

func TestPoolAutoChatCompletionsIntegration(t *testing.T) {
	t.Setenv("POOL_JWT_SECRET", "integration-secret")

	attempts := 0
	upstreamHandler := http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		attempts++

		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/v1/messages":
			_, _ = io.WriteString(w, `{
				"id":"msg_retry","type":"message","role":"assistant","model":"claude-haiku-4-5-20251001",
				"content":[{"type":"text","text":"automatic selection success"}],"stop_reason":"end_turn",
				"usage":{"input_tokens":10,"output_tokens":5}
			}`)
		case "/responses":
			_, _ = io.WriteString(w, `{
				"id":"resp_retry","object":"response","status":"completed","model":"gpt-5.4-mini",
				"output":[{"type":"message","role":"assistant","content":[{"type":"output_text","text":"automatic selection success"}]}],
				"usage":{"input_tokens":10,"output_tokens":5}
			}`)
		default:
			t.Fatalf("unexpected upstream path on retry: %s", r.URL.Path)
		}
	})

	baseURL, _ := url.Parse("http://mock-upstream.local")
	claudeAccount := &Account{
		Type:        AccountTypeClaude,
		ID:          "claude_acc_1",
		AccessToken: "test-token",
		PlanType:    "pro",
	}
	codexAccount := &Account{
		Type:        AccountTypeCodex,
		ID:          "codex_acc_auto",
		AccessToken: "test-token",
		PlanType:    "pro",
	}

	pool := newPoolState([]*Account{claudeAccount, codexAccount}, false)
	handler := &proxyHandler{
		cfg:             &config{maxInMemoryBodyBytes: 1 << 20, maxSpoolBodyBytes: 1 << 20, maxAttempts: 2, streamTimeout: 5 * time.Second},
		transport:       &mockUpstreamTransport{handler: upstreamHandler},
		pool:            pool,
		registry:        NewProviderRegistry(NewCodexProvider(baseURL, baseURL, baseURL), NewClaudeProvider(baseURL), NewGeminiProvider(baseURL, baseURL)),
		metrics:         newMetrics(),
		recent:          newRecentErrors(50),
		routeTraces:     newRouteTraceStore(100),
		poolAuto:        newPoolAutoOrchestrator(nil),
		pricing:         testPricingData(t),
	}

	// POST /v1/chat/completions with pool/auto-fast
	reqBody := `{"model":"pool/auto-fast","messages":[{"role":"user","content":"test chat"}]}`
	req := httptest.NewRequest("POST", "/v1/chat/completions", strings.NewReader(reqBody))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Authorization", "Bearer "+generateClaudePoolToken("integration-secret", "user-fast"))
	rec := httptest.NewRecorder()

	testPoolServeHTTP(t, handler, rec, req)

	if rec.Code != http.StatusOK {
		t.Fatalf("expected 200 OK after automatic selection, got %d: %s", rec.Code, rec.Body.String())
	}
	if attempts != 1 {
		t.Fatalf("expected one upstream attempt, got %d", attempts)
	}
	var completion struct {
		Object  string `json:"object"`
		Choices []struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
		} `json:"choices"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &completion); err != nil {
		t.Fatalf("decode Chat Completions response: %v", err)
	}
	if completion.Object != "chat.completion" || len(completion.Choices) != 1 ||
		completion.Choices[0].Message.Content != "automatic selection success" {
		t.Fatalf("expected translated Chat Completions reply, got %s", rec.Body.String())
	}

	// Verify route trace records decision
	reqID := rec.Header().Get("X-Pool-Request-Id")
	trace, ok := handler.routeTraces.Get(reqID)
	if !ok {
		t.Fatalf("expected trace for request")
	}
	if !strings.Contains(trace.Policy, "pool_auto:fast") {
		t.Errorf("expected policy pool_auto:fast, got %s", trace.Policy)
	}
}
