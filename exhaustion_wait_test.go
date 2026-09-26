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
)

func exhaustedCodexAccount(id string, primary float64, resetIn time.Duration) *Account {
	a := &Account{ID: id, Type: AccountTypeCodex, PlanType: "pro", AccessToken: "token", AccountID: id}
	a.Usage = UsageSnapshot{PrimaryUsedPercent: primary, PrimaryWindowMinutes: 300}
	if resetIn > 0 {
		a.Usage.PrimaryResetAt = time.Now().Add(resetIn)
	}
	return a
}

func TestWaitForUsageResetReturnsWhenWindowClears(t *testing.T) {
	acc := exhaustedCodexAccount("ex", 0.96, time.Hour)
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
	if !h.waitForUsageReset(context.Background(), AccountTypeCodex, "req-1") {
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
	if h.waitForUsageReset(context.Background(), AccountTypeCodex, "req-2") {
		t.Fatal("expected immediate give-up when the reset time is unknown")
	}
	if elapsed := time.Since(start); elapsed > time.Second {
		t.Fatalf("waited %s; expected no hold without a known reset", elapsed)
	}
}

func TestWaitForUsageResetTimesOut(t *testing.T) {
	acc := exhaustedCodexAccount("ex", 0.96, time.Hour)
	p := newPoolState([]*Account{acc}, false)
	h := &proxyHandler{cfg: &config{exhaustionWait: 60 * time.Millisecond}, pool: p}

	start := time.Now()
	if h.waitForUsageReset(context.Background(), AccountTypeCodex, "req-3") {
		t.Fatal("expected the bounded wait budget to expire")
	}
	if elapsed := time.Since(start); elapsed > 2*time.Second {
		t.Fatalf("waited %s; expected roughly the 60ms budget", elapsed)
	}
}

func TestWaitForUsageResetHonorsContextCancel(t *testing.T) {
	acc := exhaustedCodexAccount("ex", 0.96, time.Hour)
	p := newPoolState([]*Account{acc}, false)
	h := &proxyHandler{cfg: &config{exhaustionWait: 5 * time.Second}, pool: p}

	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(50 * time.Millisecond)
		cancel()
	}()
	if h.waitForUsageReset(ctx, AccountTypeCodex, "req-4") {
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

	account := exhaustedCodexAccount("acct_ex", 0.96, time.Hour)
	fx := newCodexProxyFixture(t, base, []*Account{account})
	fx.handler.cfg.exhaustionWait = 2 * time.Second
	fx.handler.aliases = newModelAliases(nil)

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
	fx.handler.aliases = newModelAliases(nil)

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
	if resp.StatusCode != http.StatusServiceUnavailable || !strings.Contains(string(body), "no live codex accounts") {
		t.Fatalf("status=%d body=%s", resp.StatusCode, body)
	}
	if elapsed := time.Since(start); elapsed > 5*time.Second {
		t.Fatalf("disabled hold waited %s; expected immediate 503", elapsed)
	}
}
