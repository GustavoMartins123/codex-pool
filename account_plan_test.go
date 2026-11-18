package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"
)

const whamUsageWithPlan = `{"plan_type":"%s","rate_limit":{"primary_window":{"used_percent":0,"reset_at":1783828800,"limit_window_seconds":18000},"secondary_window":{"used_percent":0,"reset_at":1784433600,"limit_window_seconds":604800}}}`

func TestCodexUsagePersistsUpgradedPlan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "codex.json")
	claims := base64.RawURLEncoding.EncodeToString([]byte(`{"https://api.openai.com/auth":{"chatgpt_plan_type":"plus"}}`))
	data, err := json.Marshal(map[string]any{"tokens": map[string]any{
		"id_token": "h." + claims + ".s", "access_token": jwtWithExp(time.Now().Add(240 * time.Hour).Unix()), "refresh_token": "refresh",
	}})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	provider := NewCodexProvider(nil, nil, nil)
	account, err := provider.LoadAccount("codex.json", path, data)
	if err != nil || account.PlanType != "plus" {
		t.Fatalf("loaded plan = %v, %v", account, err)
	}

	base, _ := url.Parse("https://chatgpt.com/backend-api")
	h := &proxyHandler{cfg: &config{whamBase: base, disableRefresh: true}, transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return jsonResponse(fmt.Sprintf(whamUsageWithPlan, "pro")), nil
	})}
	if err := h.fetchUsage(time.Now(), account); err != nil {
		t.Fatal(err)
	}
	if account.PlanType != "pro" {
		t.Fatalf("plan after usage = %q", account.PlanType)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := provider.LoadAccount("codex.json", path, saved)
	if err != nil || reloaded.PlanType != "pro" {
		t.Fatalf("reloaded plan = %v, %v", reloaded, err)
	}
}

func TestClaudePlanCheckPersistsUpgradedPlan(t *testing.T) {
	path := filepath.Join(t.TempDir(), "claude.json")
	data := []byte(`{"claudeAiOauth":{"accessToken":"sk-ant-oat01-test","refreshToken":"refresh","expiresAt":4102444800000,"subscriptionType":"pro","rateLimitTier":"default_claude_pro"}}`)
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
	provider := NewClaudeProvider(nil)
	account, err := provider.LoadAccount("claude.json", path, data)
	if err != nil || account.PlanType != "pro" {
		t.Fatalf("loaded plan = %v, %v", account, err)
	}
	h := &proxyHandler{transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != ClaudeOAuthProfileURL || req.Header.Get("Authorization") != "Bearer sk-ant-oat01-test" {
			t.Fatalf("unexpected profile request %s", req.URL)
		}
		return jsonResponse(`{"organization":{"organization_type":"claude_max","rate_limit_tier":"default_claude_max_20x"}}`), nil
	})}
	if err := h.checkClaudePlan(context.Background(), account); err != nil {
		t.Fatal(err)
	}
	saved, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	reloaded, err := provider.LoadAccount("claude.json", path, saved)
	if err != nil || reloaded.PlanType != "max" || reloaded.RateLimitTier != "default_claude_max_20x" {
		t.Fatalf("reloaded plan = %v, %v", reloaded, err)
	}
}

func TestAccountPlanCheckRunsOncePerInterval(t *testing.T) {
	account := &Account{ID: "acct"}
	now := time.Now()
	if !accountPlanCheckDue(account, now) || accountPlanCheckDue(account, now.Add(accountPlanCheckInterval-time.Minute)) {
		t.Fatal("plan check repeated within the interval")
	}
	if !accountPlanCheckDue(account, now.Add(accountPlanCheckInterval)) {
		t.Fatal("plan check not due after the interval")
	}
}
