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

func TestClassifyAntigravityErrors(t *testing.T) {
	tests := []struct {
		name   string
		status int
		body   string
		want   ProviderErrorClass
	}{
		{"quota reason", 429, `{"error":{"status":"RESOURCE_EXHAUSTED","details":[{"reason":"QUOTA_EXCEEDED"}]}}`, ProviderErrorQuota},
		{"rate limit reason", 429, `{"error":{"status":"RESOURCE_EXHAUSTED","details":[{"reason":"RATE_LIMIT_EXCEEDED"}]}}`, ProviderErrorQuota},
		{"session despite quota wording", 429, `{"error":{"status":"RESOURCE_EXHAUSTED","message":"quota exhausted: invalid session identifier"}}`, ProviderErrorSession},
		{"context", 429, `{"error":{"message":"context mismatch"}}`, ProviderErrorContext},
		{"input limit", 400, `{"error":{"code":"400","message":"The input token count exceeds the maximum number of tokens allowed 1048576."}}`, ProviderErrorContext},
		{"verification required", 403, `{"error":{"code":403,"message":"Verify your account to continue.","status":"PERMISSION_DENIED"}}`, ProviderErrorVerification},
		{"signature", 429, `{"error":{"message":"invalid thought signature"}}`, ProviderErrorProtocol},
		{"unknown resource exhausted", 429, `{"error":{"status":"RESOURCE_EXHAUSTED"}}`, ProviderErrorUnknown},
		{"unknown empty", 429, `{}`, ProviderErrorUnknown},
		{"capacity", 503, `{"error":{"message":"No capacity available for model"}}`, ProviderErrorCapacity},
		{"auth", 401, `{"error":{"message":"invalid token"}}`, ProviderErrorAuth},
		{"policy", 403, `{"error":{"message":"policy violation"}}`, ProviderErrorPolicy},
		{"transient", 502, `{"error":{"message":"upstream unavailable"}}`, ProviderErrorTransient},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := classifyAntigravityError(test.status, []byte(test.body))
			if got.Class != test.want || got.StatusCode != test.status {
				t.Fatalf("classify = %+v, want class=%s status=%d", got, test.want, test.status)
			}
		})
	}
}

func TestAntigravity429CooldownRequiresQuotaEvidence(t *testing.T) {
	for _, test := range []struct {
		name     string
		body     string
		cooldown bool
	}{
		{"session", `{"error":{"status":"RESOURCE_EXHAUSTED","message":"invalid session identifier"}}`, false},
		{"context", `{"error":{"status":"RESOURCE_EXHAUSTED","message":"context mismatch"}}`, false},
		{"signature", `{"error":{"status":"RESOURCE_EXHAUSTED","message":"invalid thought signature"}}`, false},
		{"unknown", `{"error":{"status":"RESOURCE_EXHAUSTED"}}`, false},
		{"quota", `{"error":{"status":"RESOURCE_EXHAUSTED","details":[{"reason":"QUOTA_EXCEEDED"}]}}`, true},
	} {
		t.Run(test.name, func(t *testing.T) {
			daily, _ := url.Parse("https://daily.example")
			prod, _ := url.Parse("https://prod.example")
			provider := NewAntigravityProvider(daily, prod)
			account := &Account{Type: AccountTypeAntigravity, ID: "anti", AccessToken: "token", ProjectID: "project", PlanType: "pro"}
			h := &proxyHandler{
				cfg:      &config{maxAttempts: 1, requestTimeout: time.Second, streamTimeout: time.Second},
				pool:     newPoolState([]*Account{account}, false),
				registry: NewProviderRegistry(nil, nil, nil, provider),
				transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
					return &http.Response{StatusCode: 429, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(test.body)), Request: req}, nil
				}),
			}
			r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"antigravity/gemini-3.8-flash-high","input":"hello","stream":false}`))
			w := httptest.NewRecorder()
			if !h.handleAntigravityProxy(w, r, []byte(`{"model":"antigravity/gemini-3.8-flash-high","input":"hello","stream":false}`), "antigravity/gemini-3.8-flash-high", "conv-429", "user", "origin", "127.0.0.1", "req", "") {
				t.Fatal("Antigravity request was not handled")
			}
			if w.Code != 429 {
				t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
			}
			account.mu.Lock()
			count := len(account.ModelRateLimits)
			until := account.RateLimitUntil
			account.mu.Unlock()
			if (count > 0) != test.cooldown || !until.IsZero() {
				t.Fatalf("cooldown count=%d account rate limit=%s want cooldown=%v", count, until, test.cooldown)
			}
		})
	}
}

func TestGeneric429SessionErrorDoesNotConsumeQuota(t *testing.T) {
	t.Setenv("POOL_JWT_SECRET", "generic-429-secret")
	base, _ := url.Parse("https://codex.mock")
	account := &Account{Type: AccountTypeCodex, ID: "codex", AccessToken: "token", PlanType: "pro"}
	h := &proxyHandler{
		cfg:      &config{maxAttempts: 1, maxInMemoryBodyBytes: 1 << 20, requestTimeout: time.Second, streamTimeout: time.Second},
		pool:     newPoolState([]*Account{account}, false),
		registry: NewProviderRegistry(NewCodexProvider(base, base, nil), nil, nil),
		metrics:  newMetrics(), recent: newRecentErrors(10),
		transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			body := `{"error":{"status":"RESOURCE_EXHAUSTED","message":"invalid session identifier"}}`
			return &http.Response{StatusCode: 429, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
		}),
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.6-sol","conversation_id":"unknown-429","input":"hello","stream":false}`))
	r.Header.Set("Authorization", "Bearer "+generateClaudePoolToken("generic-429-secret", "user"))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	testPoolServeHTTP(t, h, w, r)
	if w.Code != 429 || !strings.Contains(w.Body.String(), "invalid session") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	account.mu.Lock()
	until := account.RateLimitUntil
	account.mu.Unlock()
	if !until.IsZero() {
		t.Fatalf("session error set quota cooldown: %s", until)
	}
}

func TestCodexItemReference404DoesNotPenalizeAccount(t *testing.T) {
	t.Setenv("POOL_JWT_SECRET", "item-ref-secret")
	base, _ := url.Parse("https://codex.mock")
	account := &Account{Type: AccountTypeCodex, ID: "codex", AccessToken: "token", PlanType: "pro"}
	calls := 0
	h := &proxyHandler{
		cfg:      &config{maxAttempts: 3, maxInMemoryBodyBytes: 1 << 20, requestTimeout: time.Second, streamTimeout: time.Second},
		pool:     newPoolState([]*Account{account}, false),
		registry: NewProviderRegistry(NewCodexProvider(base, base, nil), nil, nil),
		metrics:  newMetrics(), recent: newRecentErrors(10),
		transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			calls++
			if calls == 1 {
				body := `{"detail":"Item with id 'rs_resp_5Qi8asrpEcPO-8YPq6-xiQw_0' not found. Items are not persisted when ` + "`store`" + ` is set to false. Try again with ` + "`store`" + ` set to true, or remove this item from your input."}`
				return &http.Response{StatusCode: http.StatusNotFound, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(body)), Request: req}, nil
			}
			response := `data: {"type":"response.completed"}` + "\n\n"
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(response)), Request: req}, nil
		}),
	}
	body := `{"model":"gpt-5.6-sol","conversation_id":"resumed-session","input":"hello","stream":false}`
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	r.Header.Set("Authorization", "Bearer "+generateClaudePoolToken("item-ref-secret", "user"))
	r.Header.Set("Content-Type", "application/json")
	w := httptest.NewRecorder()
	testPoolServeHTTP(t, h, w, r)
	if w.Code != http.StatusNotFound || !strings.Contains(w.Body.String(), "Item with id") {
		t.Fatalf("status=%d body=%s", w.Code, w.Body.String())
	}
	if calls != 1 {
		t.Fatalf("item-reference 404 retried upstream: calls=%d", calls)
	}
	account.mu.Lock()
	penalty := account.Penalty
	account.mu.Unlock()
	if penalty != 0 {
		t.Fatalf("item-reference 404 penalized account: penalty=%.2f", penalty)
	}
	r2 := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	r2.Header.Set("Authorization", "Bearer "+generateClaudePoolToken("item-ref-secret", "user"))
	r2.Header.Set("Content-Type", "application/json")
	w2 := httptest.NewRecorder()
	testPoolServeHTTP(t, h, w2, r2)
	if w2.Code != http.StatusOK || calls != 2 {
		t.Fatalf("follow-up request status=%d calls=%d body=%s", w2.Code, calls, w2.Body.String())
	}
}

func TestAntigravityRetriesInputLimitWithCompactedBody(t *testing.T) {
	daily, _ := url.Parse("https://daily.example")
	prod, _ := url.Parse("https://prod.example")
	account := &Account{Type: AccountTypeAntigravity, ID: "anti", AccessToken: "token", ProjectID: "project", PlanType: "pro"}
	calls := 0
	var requestSizes []int
	h := &proxyHandler{
		cfg:      &config{maxAttempts: 1, requestTimeout: time.Second, streamTimeout: time.Second},
		pool:     newPoolState([]*Account{account}, false),
		registry: NewProviderRegistry(nil, nil, nil, NewAntigravityProvider(daily, prod)),
		transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			raw, _ := io.ReadAll(req.Body)
			calls++
			requestSizes = append(requestSizes, len(raw))
			if calls == 1 {
				return &http.Response{StatusCode: http.StatusBadRequest, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"code":"400","message":"The input token count exceeds the maximum number of tokens allowed 1048576."}}`)), Request: req}, nil
			}
			if !strings.Contains(string(raw), "Earlier conversation compacted") {
				t.Errorf("retry did not compact the request: %s", raw)
			}
			response := `data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}}` + "\n\n"
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(response)), Request: req}, nil
		}),
	}
	body, err := json.Marshal(map[string]any{
		"model":  "antigravity/gemini-3.8-flash-high",
		"stream": false,
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": strings.Repeat("retry-context-", 260_000)}}},
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "latest request"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(string(body)))
	w := httptest.NewRecorder()
	if !h.handleAntigravityProxy(w, r, body, "antigravity/gemini-3.8-flash-high", "", "user", "origin", "127.0.0.1", "req", "") {
		t.Fatal("Antigravity request was not handled")
	}
	if w.Code != http.StatusOK || calls != 2 || len(requestSizes) != 2 || requestSizes[1] >= requestSizes[0] {
		t.Fatalf("status=%d calls=%d sizes=%v body=%s", w.Code, calls, requestSizes, w.Body.String())
	}
}
