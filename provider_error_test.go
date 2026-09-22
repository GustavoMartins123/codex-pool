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
	h.ServeHTTP(w, r)
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
