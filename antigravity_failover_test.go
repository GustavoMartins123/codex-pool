package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"sync"
	"testing"
	"time"
)

type antigravityFailoverHarness struct {
	handler *proxyHandler
	mu      sync.Mutex
	tokens  []string
}

// newAntigravityFailoverHarness answers each account by its access token.
// Accounts listed later carry more primary usage, so routing tries them last.
func newAntigravityFailoverHarness(t *testing.T, statuses map[string]int) (*antigravityFailoverHarness, []*Account) {
	t.Helper()
	antigravityModels.Reset()
	t.Cleanup(antigravityModels.Reset)
	daily, _ := url.Parse("https://daily.example")
	prod, _ := url.Parse("https://prod.example")
	var accounts []*Account
	for _, token := range []string{"a", "b", "c", "d", "e"} {
		if _, ok := statuses[token]; !ok {
			continue
		}
		accounts = append(accounts, &Account{
			Type: AccountTypeAntigravity, ID: "anti-" + token, AccessToken: token, ProjectID: "project", PlanType: "pro",
			ExpiresAt: time.Now().Add(24 * time.Hour), ModelRateLimits: map[string]time.Time{},
			Usage: UsageSnapshot{PrimaryUsedPercent: 0.1 * float64(len(accounts)), PrimaryUsed: 0.1 * float64(len(accounts))},
		})
	}
	harness := &antigravityFailoverHarness{}
	harness.handler = &proxyHandler{
		cfg:      &config{maxAttempts: 1, requestTimeout: time.Second, streamTimeout: time.Second},
		pool:     newPoolState(accounts, false),
		registry: NewProviderRegistry(nil, nil, nil, NewAntigravityProvider(daily, prod)),
		transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			token := strings.TrimPrefix(req.Header.Get("Authorization"), "Bearer ")
			harness.mu.Lock()
			if len(harness.tokens) == 0 || harness.tokens[len(harness.tokens)-1] != token {
				harness.tokens = append(harness.tokens, token)
			}
			harness.mu.Unlock()
			status := statuses[token]
			if status != http.StatusOK {
				return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"message":"upstream failure"}}`)), Request: req}, nil
			}
			response := `data: {"response":{"candidates":[{"content":{"role":"model","parts":[{"text":"ok"}]},"finishReason":"STOP"}]}}` + "\n\n"
			return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(response)), Request: req}, nil
		}),
	}
	return harness, accounts
}

func (h *antigravityFailoverHarness) send(t *testing.T) *httptest.ResponseRecorder {
	t.Helper()
	body := `{"model":"antigravity/gemini-3.8-flash-high","input":"hello","stream":false}`
	r := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
	w := httptest.NewRecorder()
	if !h.handler.handleAntigravityProxy(w, r, []byte(body), "antigravity/gemini-3.8-flash-high", "", "user", "origin", "127.0.0.1", "req", "") {
		t.Fatal("Antigravity model was not handled")
	}
	return w
}

func TestAntigravityRotatesAfterUpstreamServerError(t *testing.T) {
	harness, _ := newAntigravityFailoverHarness(t, map[string]int{"a": http.StatusInternalServerError, "b": http.StatusOK})
	w := harness.send(t)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s, want the second account's answer", w.Code, w.Body.String())
	}
	if strings.Join(harness.tokens, ",") != "a,b" {
		t.Fatalf("accounts tried = %v, want a then b", harness.tokens)
	}
}

func TestAntigravityRotatesAfterUnrecoverableUnauthorized(t *testing.T) {
	harness, _ := newAntigravityFailoverHarness(t, map[string]int{"a": http.StatusUnauthorized, "b": http.StatusOK})
	w := harness.send(t)
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s, want the second account's answer", w.Code, w.Body.String())
	}
	if strings.Join(harness.tokens, ",") != "a,b" {
		t.Fatalf("accounts tried = %v, want a then b", harness.tokens)
	}
}

func TestAntigravityServerErrorFailoverIsBounded(t *testing.T) {
	statuses := map[string]int{}
	for _, token := range []string{"a", "b", "c", "d", "e"} {
		statuses[token] = http.StatusInternalServerError
	}
	harness, _ := newAntigravityFailoverHarness(t, statuses)
	w := harness.send(t)
	if w.Code != http.StatusInternalServerError {
		t.Fatalf("status = %d body = %s, want the upstream 500 relayed", w.Code, w.Body.String())
	}
	if len(harness.tokens) != antigravityMaxServerFailover {
		t.Fatalf("accounts tried = %v, want %d", harness.tokens, antigravityMaxServerFailover)
	}
}

func TestAntigravityQuotaCooldownScopesToNamedFamily(t *testing.T) {
	familyBody := []byte(`{"error":{"status":"RESOURCE_EXHAUSTED","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"QUOTA_EXHAUSTED","metadata":{"quota_bucket":"gemini-weekly"}}]}}`)
	if got := antigravityRateLimitScope("gemini-3.8-flash-high", familyBody); got != "family:gemini" {
		t.Fatalf("scope = %q, want the gemini family", got)
	}
	if got := antigravityRateLimitScope("claude-opus-4-6-thinking", familyBody); got != "claude-opus-4-6-thinking" {
		t.Fatalf("another family's bucket must not widen the scope, got %q", got)
	}
	if got := antigravityRateLimitScope("gemini-3.8-flash-high", []byte(`{"error":{"status":"RESOURCE_EXHAUSTED","message":"quota exceeded for gemini-weekly"}}`)); got != "gemini-3.8-flash-high" {
		t.Fatalf("free-form text must stay model scoped, got %q", got)
	}

	until := time.Now().Add(time.Hour)
	account := &Account{Type: AccountTypeAntigravity, ID: "ag", ModelRateLimits: map[string]time.Time{}}
	setAntigravityQuotaCooldown(account, "gemini-3.8-flash-high", familyBody, until)
	account.mu.Lock()
	sibling := antigravityModelCooldownLocked(account, "gemini-3.5-flash-low")
	other := antigravityModelCooldownLocked(account, "claude-opus-4-6-thinking")
	account.mu.Unlock()
	if !sibling.Equal(until) || !other.IsZero() {
		t.Fatalf("family cooldown: sibling=%s other=%s", sibling, other)
	}
	clearAntigravityModelCooldown(account, "gemini-3.5-flash-low")
	account.mu.Lock()
	sibling = antigravityModelCooldownLocked(account, "gemini-3.8-flash-high")
	account.mu.Unlock()
	if !sibling.IsZero() {
		t.Fatal("a served family model must clear the family cooldown")
	}
}
