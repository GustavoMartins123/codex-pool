package main

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"codex-pool-proxy/internal/modelalias"
)

func exhaustedCodexAccount(id string, primary float64, resetIn time.Duration) *Account {
	a := &Account{ID: id, Type: AccountTypeCodex, PlanType: "pro", AccessToken: "token", codex: codexAccountState{AccountID: id}}
	a.Usage = UsageSnapshot{PrimaryUsedPercent: primary, PrimaryWindowMinutes: 300}
	if resetIn > 0 {
		a.Usage.PrimaryResetAt = time.Now().Add(resetIn)
	}
	return a
}

func TestWaitForUsageResetReturnsWhenWindowClears(t *testing.T) {
	acc := exhaustedCodexAccount("ex", 0.96, time.Second)
	p := newPoolState([]*Account{acc}, false)
	h := &proxyHandler{cfg: &config{exhaustionWait: 5 * time.Second}, pool: p}

	// Simulate the periodic usage poll observing the window reset.
	go func() {
		time.Sleep(80 * time.Millisecond)
		acc.mu.Lock()
		acc.Usage.PrimaryUsedPercent = 0.1
		acc.mu.Unlock()
	}()

	start := time.Now()
	if !h.waitForUsageReset(context.Background(), AccountTypeCodex, "req-1", nil) {
		t.Fatal("expected wait to end once the window cleared")
	}
	if elapsed := time.Since(start); elapsed < 50*time.Millisecond {
		t.Fatalf("returned too quickly (%s); expected the request to be held", elapsed)
	}
}

func TestWaitForUsageResetGivesUpWithoutKnownReset(t *testing.T) {
	acc := exhaustedCodexAccount("ex", 0.96, 0)
	p := newPoolState([]*Account{acc}, false)
	h := &proxyHandler{cfg: &config{exhaustionWait: 5 * time.Second}, pool: p}

	start := time.Now()
	if h.waitForUsageReset(context.Background(), AccountTypeCodex, "req-2", nil) {
		t.Fatal("expected immediate give-up when the reset time is unknown")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("waited %s; expected no hold without a known reset", elapsed)
	}
}

func TestWaitForUsageResetTimesOut(t *testing.T) {
	acc := exhaustedCodexAccount("ex", 0.96, 50*time.Millisecond)
	p := newPoolState([]*Account{acc}, false)
	h := &proxyHandler{cfg: &config{exhaustionWait: 60 * time.Millisecond}, pool: p}

	start := time.Now()
	if h.waitForUsageReset(context.Background(), AccountTypeCodex, "req-3", nil) {
		t.Fatal("expected the bounded wait budget to expire")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("waited %s; expected roughly the 60ms budget", elapsed)
	}
}

func TestWaitForUsageResetHonorsContextCancel(t *testing.T) {
	acc := exhaustedCodexAccount("ex", 0.96, time.Second)
	p := newPoolState([]*Account{acc}, false)
	h := &proxyHandler{cfg: &config{exhaustionWait: 5 * time.Second}, pool: p}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	if h.waitForUsageReset(ctx, AccountTypeCodex, "req-4", nil) {
		t.Fatal("expected cancellation to end the hold")
	}
}

func TestProxyHoldsRequestForUsageWindowReset(t *testing.T) {
	t.Setenv("POOL_JWT_SECRET", "test-secret")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"type\":\"response.completed\"}\n\n"))
	}))
	defer upstream.Close()
	base, _ := url.Parse(upstream.URL)

	account := exhaustedCodexAccount("acct_ex", 0.96, time.Second)
	fx := newCodexProxyFixture(t, base, []*Account{account})
	fx.handler.cfg.exhaustionWait = 2 * time.Second
	fx.handler.aliases = modelalias.New(nil)

	// Simulate the periodic usage poll observing the window reset shortly
	// after the request arrives.
	go func() {
		time.Sleep(120 * time.Millisecond)
		account.mu.Lock()
		account.Usage.PrimaryUsedPercent = 0.1
		account.Usage.PrimaryResetAt = time.Now().Add(5 * time.Hour)
		account.mu.Unlock()
	}()

	payload := `{"model":"gpt-5.5","stream":true,"input":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest(http.MethodPost, fx.server.URL+"/v1/responses", strings.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+generateClaudePoolToken("test-secret", "test-user"))
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("request was not held while the account was usage-exhausted (elapsed %s)", elapsed)
	}
}

func TestProxyStillFailsFastWhenExhaustionWaitDisabled(t *testing.T) {
	t.Setenv("POOL_JWT_SECRET", "test-secret")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("upstream must not be reached while the account is usage-exhausted")
	}))
	defer upstream.Close()
	base, _ := url.Parse(upstream.URL)

	account := exhaustedCodexAccount("acct_ex", 0.96, time.Hour)
	fx := newCodexProxyFixture(t, base, []*Account{account})
	fx.handler.aliases = modelalias.New(nil)

	payload := `{"model":"gpt-5.5","stream":true,"input":[{"role":"user","content":"hi"}]}`
	req, _ := http.NewRequest(http.MethodPost, fx.server.URL+"/v1/responses", strings.NewReader(payload))
	req.Header.Set("Authorization", "Bearer "+generateClaudePoolToken("test-secret", "test-user"))
	req.Header.Set("Content-Type", "application/json")

	start := time.Now()
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	body, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusTooManyRequests || !strings.Contains(string(body), "provider_quota_exhausted") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("disabled hold waited %s; expected immediate 503", elapsed)
	}
}

func TestUsageResetWithinBudget(t *testing.T) {
	exhaustedSoon := exhaustedCodexAccount("soon", 0.96, 200*time.Millisecond)
	p := newPoolState([]*Account{exhaustedSoon}, false)

	if !p.usageResetWithinBudget(AccountTypeCodex, time.Second, nil) {
		t.Fatal("expected a reset within the 1s budget")
	}
	if p.usageResetWithinBudget(AccountTypeCodex, 50*time.Millisecond, nil) {
		t.Fatal("expected the reset to exceed the 50ms budget")
	}

	recovered := exhaustedCodexAccount("ok", 0.1, time.Hour)
	p2 := newPoolState([]*Account{recovered}, false)
	if p2.usageResetWithinBudget(AccountTypeCodex, time.Hour, nil) {
		t.Fatal("expected false while an account is routable")
	}
}

// newExhaustedCodexHandler builds a pool with exhausted Codex accounts and a live Antigravity account.
func newExhaustedCodexHandler(t *testing.T, codexAccounts ...*Account) *proxyHandler {
	t.Helper()
	t.Setenv("POOL_JWT_SECRET", "test-secret")

	codexBase, _ := url.Parse("https://chatgpt.com/backend-api/codex")
	claudeBase, _ := url.Parse("https://api.anthropic.com")
	geminiBase, _ := url.Parse("https://generativelanguage.googleapis.com")
	antiDaily, _ := url.Parse("https://daily.example")
	antiProd, _ := url.Parse("https://prod.example")

	antiAcc := &Account{Type: AccountTypeAntigravity, ID: "anti-1", AccessToken: "anti-token", google: googleAccountState{ProjectID: "proj-1"}, PlanType: "pro"}
	accounts := append(append([]*Account{}, codexAccounts...), antiAcc)
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		payload := "data: {\"type\":\"response.completed\",\"servedBy\":\"anti\"}\n\n"
		if strings.Contains(req.URL.Host, "chatgpt.com") {
			payload = "data: {\"type\":\"response.completed\",\"servedBy\":\"codex\"}\n\n"
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader(payload)),
			Request:    req,
		}, nil
	})
	return &proxyHandler{
		cfg: &config{
			requestTimeout:       5 * time.Second,
			streamTimeout:        5 * time.Second,
			maxInMemoryBodyBytes: 4 << 20,
			maxAttempts:          1,
			exhaustionWait:       2 * time.Second,
		},
		transport: transport,
		pool:      newPoolState(accounts, false),
		registry: NewProviderRegistry(
			NewCodexProvider(codexBase, codexBase, nil),
			NewClaudeProvider(claudeBase),
			NewGeminiProvider(geminiBase, geminiBase),
			NewAntigravityProvider(antiDaily, antiProd),
		),
		metrics: newMetrics(),
		recent:  newRecentErrors(5),
	}
}

func recoverExhaustedAccountAfter(t *testing.T, acc *Account, delay time.Duration) {
	t.Helper()
	go func() {
		time.Sleep(delay)
		acc.mu.Lock()
		acc.Usage.PrimaryUsedPercent = 0.1
		acc.Usage.PrimaryResetAt = time.Now().Add(5 * time.Hour)
		acc.mu.Unlock()
	}()
}

// A reset within the configured wait can restore the requested provider's capacity.
func TestProxyWaitsForRequestedProvidersReset(t *testing.T) {
	account := exhaustedCodexAccount("acct_ex", 0.96, 400*time.Millisecond)
	h := newExhaustedCodexHandler(t, account)
	recoverExhaustedAccountAfter(t, account, 120*time.Millisecond)

	start := time.Now()
	w := postCodexModelRequest(t, h, "gpt-5.6-sol")
	if w.Code != http.StatusOK {
		t.Fatalf("status = %d, want 200 (body=%q)", w.Code, w.Body.String())
	}
	if !strings.Contains(w.Body.String(), "\"servedBy\":\"codex\"") {
		t.Fatalf("expected the original codex provider to answer, got %q", w.Body.String())
	}
	if elapsed := time.Since(start); elapsed < 100*time.Millisecond {
		t.Fatalf("request was not held while the account was usage-exhausted (elapsed %s)", elapsed)
	}
}

func TestProxyRejectsDistantReset(t *testing.T) {
	account := exhaustedCodexAccount("acct_ex", 0.96, time.Hour)
	h := newExhaustedCodexHandler(t, account)
	start := time.Now()
	w := postCodexModelRequest(t, h, "gpt-5.6-sol")
	if w.Code != http.StatusTooManyRequests || !strings.Contains(w.Body.String(), "provider_quota_exhausted") {
		t.Fatalf("expected an explicit quota error, got %d %q", w.Code, w.Body.String())
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("waited for a reset outside the hold budget (%s)", elapsed)
	}
}
