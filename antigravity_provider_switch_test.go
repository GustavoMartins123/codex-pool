package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"
)

func TestConversationSwitchCodexToAntigravity(t *testing.T) {
	t.Setenv("POOL_JWT_SECRET", "test-secret")

	codexBase, _ := url.Parse("https://chatgpt.com/backend-api/codex")
	claudeBase, _ := url.Parse("https://api.anthropic.com")
	geminiBase, _ := url.Parse("https://generativelanguage.googleapis.com")
	antigravityDaily, _ := url.Parse("https://daily.example")
	antigravityProd, _ := url.Parse("https://prod.example")

	codexProvider := NewCodexProvider(codexBase, codexBase, nil)
	claudeProvider := NewClaudeProvider(claudeBase)
	geminiProvider := NewGeminiProvider(geminiBase, geminiBase)
	antigravityProvider := NewAntigravityProvider(antigravityDaily, antigravityProd)

	codexAcc := &Account{Type: AccountTypeCodex, ID: "codex-1", AccessToken: "codex-token", PlanType: "pro"}
	antiAcc := &Account{Type: AccountTypeAntigravity, ID: "anti-1", AccessToken: "anti-token", ProjectID: "proj-1", PlanType: "pro"}

	pool := newPoolState([]*Account{codexAcc, antiAcc}, false)
	registry := NewProviderRegistry(codexProvider, claudeProvider, geminiProvider, antigravityProvider)

	isValidAntigravitySessionID := func(id string) bool {
		id = strings.TrimSpace(id)
		if !strings.HasPrefix(id, "-") {
			return false
		}
		val, err := strconv.ParseInt(id, 10, 64)
		return err == nil && val < 0
	}

	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if strings.Contains(req.URL.Host, "chatgpt.com") {
			body := `data: {"type":"response.completed","response":{"id":"resp_codex_1","conversation_id":"conv-X","status":"completed","output":[]}}` + "\n\n"
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(body)),
				Request:    req,
			}, nil
		}
		if strings.Contains(req.URL.Host, "example") {
			reqBody, _ := io.ReadAll(req.Body)
			var root map[string]any
			_ = json.Unmarshal(reqBody, &root)
			requestMap, _ := root["request"].(map[string]any)
			sessionID, _ := requestMap["sessionId"].(string)

			// Upstream Antigravity rejects incompatible session IDs or previous provider context with 429
			if !isValidAntigravitySessionID(sessionID) {
				rateLimitBody := `{"error":{"code":429,"message":"quota exhausted: invalid session identifier","status":"RESOURCE_EXHAUSTED"}}`
				return &http.Response{
					StatusCode: http.StatusTooManyRequests,
					Header:     http.Header{"Content-Type": []string{"application/json"}},
					Body:       io.NopCloser(strings.NewReader(rateLimitBody)),
					Request:    req,
				}, nil
			}

			respSSE := `data: {"response":{"responseId":"anti-resp-1","candidates":[{"content":{"parts":[{"text":"hello from antigravity"}]},"finishReason":"STOP"}]}}` + "\n\n"
			return &http.Response{
				StatusCode: http.StatusOK,
				Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
				Body:       io.NopCloser(strings.NewReader(respSSE)),
				Request:    req,
			}, nil
		}
		return &http.Response{StatusCode: http.StatusNotFound, Body: io.NopCloser(strings.NewReader("not found")), Request: req}, nil
	})

	h := &proxyHandler{
		cfg: &config{
			requestTimeout:       5 * time.Second,
			streamTimeout:        5 * time.Second,
			maxInMemoryBodyBytes: 4 << 20,
			maxAttempts:          1,
		},
		transport: transport,
		pool:      pool,
		registry:  registry,
		metrics:   newMetrics(),
		recent:    newRecentErrors(5),
	}

	token := generateClaudePoolToken("test-secret", "user-1")

	// Request 1: model = gpt-5.6-sol, conversation = conv-X -> 200
	req1Body := `{"model":"gpt-5.6-sol","conversation_id":"conv-X","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"hello"}]}],"stream":true}`
	req1 := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(req1Body))
	req1.Header.Set("Authorization", "Bearer "+token)
	req1.Header.Set("Content-Type", "application/json")
	w1 := httptest.NewRecorder()

	h.ServeHTTP(w1, req1)
	if w1.Code != http.StatusOK {
		t.Fatalf("Request 1 status = %d, want 200", w1.Code)
	}

	// Verify conversation was initially pinned to Codex
	pool.mu.Lock()
	initialPin := pool.convPin["conv-X"]
	pool.mu.Unlock()
	if initialPin != "codex-1" {
		t.Fatalf("conversation conv-X pinned to %q, want codex-1", initialPin)
	}

	// Request 2: model = antigravity/gemini-3.8-flash-high, conversation = conv-X
	req2Body := `{"model":"antigravity/gemini-3.8-flash-high","conversation_id":"conv-X","previous_response_id":"resp_codex_1","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"next question"}]}],"stream":true}`
	req2 := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(req2Body))
	req2.Header.Set("Authorization", "Bearer "+token)
	req2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()

	h.ServeHTTP(w2, req2)
	t.Logf("Request 2 status: %d", w2.Code)

	if w2.Code != http.StatusOK {
		t.Errorf("Request 2 status = %d, want 200", w2.Code)
	}

	// Verify conversation pin transitioned to the new Antigravity account
	pool.mu.Lock()
	newPin := pool.convPin["conv-X"]
	modelPin := pool.convPin["antigravity:gemini-3.8-flash-high:conv-X"]
	pool.mu.Unlock()
	if newPin != "anti-1" {
		t.Errorf("conversation conv-X pinned to %q, want anti-1", newPin)
	}
	if modelPin != "anti-1" {
		t.Errorf("model pin = %q, want anti-1", modelPin)
	}

	// Verify response does not echo the old provider's previous_response_id
	if strings.Contains(w2.Body.String(), "resp_codex_1") {
		t.Errorf("response leaked previous provider response ID: %s", w2.Body.String())
	}
}
