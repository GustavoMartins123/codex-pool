package main

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strconv"
	"strings"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

func TestUsageWaitRejectsResetOutsideBudget(t *testing.T) {
	account := exhaustedCodexAccount("exhausted", 1, 2*time.Hour)
	h := &proxyHandler{cfg: &config{exhaustionWait: 200 * time.Millisecond}, pool: newPoolState([]*Account{account}, false)}
	start := time.Now()
	if h.waitForUsageReset(context.Background(), AccountTypeCodex, "outside-budget", nil) {
		t.Fatal("a reset outside the hold budget must not become available")
	}
	if elapsed := time.Since(start); elapsed >= 100*time.Millisecond {
		t.Fatalf("waited %s for a reset two hours away", elapsed)
	}
}

func TestUsageResetRequiresEveryExhaustedWindowToClear(t *testing.T) {
	account := exhaustedCodexAccount("two-windows", 1, time.Minute)
	account.Usage.SecondaryUsedPercent = 1
	account.Usage.SecondaryResetAt = time.Now().Add(2 * time.Hour)
	pool := newPoolState([]*Account{account}, false)
	reset, known := pool.nearestUsageReset(AccountTypeCodex, nil)
	if !known || reset < 119*time.Minute || reset > 2*time.Hour {
		t.Fatalf("reset = %s, known = %v; both exhausted windows must clear", reset, known)
	}
	account.Usage.SecondaryResetAt = time.Time{}
	if _, known := pool.nearestUsageReset(AccountTypeCodex, nil); known {
		t.Fatal("an unknown exhausted window must not advertise a usable reset")
	}
}

func TestUsageWaitHonorsDeadlineAndExcludedAccounts(t *testing.T) {
	exhausted := exhaustedCodexAccount("exhausted", 1, time.Second)
	healthy := &Account{ID: "foreign", Type: AccountTypeCodex}
	h := &proxyHandler{cfg: &config{exhaustionWait: 5 * time.Second}, pool: newPoolState([]*Account{exhausted, healthy}, false)}
	ctx, cancel := context.WithTimeout(context.Background(), 100*time.Millisecond)
	defer cancel()
	start := time.Now()
	if h.waitForUsageReset(ctx, AccountTypeCodex, "scoped-deadline", map[string]bool{"foreign": true}) {
		t.Fatal("an excluded healthy account must not release the request")
	}
	if time.Since(start) >= 50*time.Millisecond {
		t.Fatal("a reset beyond the request deadline must not hold the request")
	}
}

func TestQuotaErrorDoesNotExposeForeignAccountReset(t *testing.T) {
	t.Setenv("POOL_JWT_SECRET", "test-secret")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("unavailable or foreign accounts must not receive traffic")
	}))
	defer upstream.Close()
	base, _ := url.Parse(upstream.URL)
	owned := exhaustedCodexAccount("owned", 1, 0)
	foreign := exhaustedCodexAccount("foreign", 1, 2*time.Hour)
	fx := newCodexProxyFixture(t, base, []*Account{owned, foreign})
	h := fx.handler
	h.cfg.exhaustionWait = 50 * time.Millisecond
	h.aliases = newModelAliases(nil)
	addTestPassportOperator(t, h.passport, "alice")
	addTestPassportOperator(t, h.passport, "bob")
	if err := h.passport.initializeAccountAuthority(nil); err != nil {
		t.Fatal(err)
	}
	if err := h.passport.db.Update(func(tx *bbolt.Tx) error {
		for _, item := range []struct {
			account *Account
			owner   string
		}{{owned, "alice"}, {foreign, "bob"}} {
			if err := putJSON(tx.Bucket([]byte(bucketAccountResources)), resourceKey(item.account.Type, item.account.ID),
				accountResource{Version: 2, ID: item.account.ID, Provider: item.account.Type, OwnerID: item.owner, AddedBy: item.owner, Status: "active", Revision: 1}); err != nil {
				return err
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	h.pool.accountAuthority = h.passport
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, fx.server.URL+"/v1/responses", strings.NewReader(`{"model":"gpt-5.5","input":"synthetic","stream":true}`))
	req.Header.Set("Authorization", "Bearer "+generateClaudePoolToken("test-secret", "alice"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusServiceUnavailable || resp.Header.Get("Retry-After") != "" {
		t.Fatalf("foreign quota influenced the response: status %d, retry %q", resp.StatusCode, resp.Header.Get("Retry-After"))
	}
}

func TestClaudeExhaustedQuotaReturnsErrorWithoutHolding(t *testing.T) {
	t.Setenv("POOL_JWT_SECRET", "test-secret")
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		t.Error("an exhausted account must not receive upstream traffic")
	}))
	defer upstream.Close()
	base, _ := url.Parse(upstream.URL)
	account := &Account{ID: "claude-exhausted", Type: AccountTypeClaude, AccessToken: "synthetic", PlanType: "max",
		Usage: UsageSnapshot{PrimaryUsedPercent: 1, PrimaryResetAt: time.Now().Add(2 * time.Hour)}}
	syncUsageCooldown(account)
	fx := newCodexProxyFixture(t, base, []*Account{account})
	fx.handler.cfg.exhaustionWait = 100 * time.Millisecond
	fx.handler.cfg.exhaustionPreferWait = true
	fx.handler.aliases = newModelAliases(nil)

	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	req, _ := http.NewRequestWithContext(ctx, http.MethodPost, fx.server.URL+"/v1/responses", strings.NewReader(`{"model":"claude-sonnet-5-5","input":"synthetic","stream":true}`))
	req.Header.Set("Authorization", "Bearer "+generateClaudePoolToken("test-secret", "test-user"))
	req.Header.Set("Content-Type", "application/json")
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatalf("expected an explicit quota error, got %v", err)
	}
	defer resp.Body.Close()
	var body struct {
		Error struct {
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&body); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusTooManyRequests || body.Error.Code != "provider_quota_exhausted" {
		t.Fatalf("status = %d, code = %q", resp.StatusCode, body.Error.Code)
	}
	seconds, err := strconv.Atoi(resp.Header.Get("Retry-After"))
	if err != nil || seconds < 7190 || seconds > 7200 {
		t.Fatalf("invalid Retry-After: %q", resp.Header.Get("Retry-After"))
	}
}
