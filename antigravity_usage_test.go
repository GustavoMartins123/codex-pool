package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestAntigravityUsageAndRemainingTokensTracking(t *testing.T) {
	resetTime := time.Now().Add(12 * time.Hour).UTC().Truncate(time.Second)
	remFraction := 0.75

	snapshot := AntigravityAccountSnapshot{
		FetchedAt: time.Now(),
		Models: map[string]AntigravityModelInfo{
			"gemini-3.8-flash-high": {
				ID: "gemini-3.8-flash-high",
				Quota: AntigravityQuotaInfo{
					RemainingFraction: &remFraction,
					ResetTime:         resetTime,
				},
			},
		},
	}

	usage := extractAntigravityAccountUsage(snapshot)

	// Remaining is 75%, so used is 25% (0.25)
	if usage.PrimaryUsedPercent != 0.25 {
		t.Errorf("PrimaryUsedPercent = %v, want 0.25", usage.PrimaryUsedPercent)
	}
	if usage.PrimaryResetAt != resetTime {
		t.Errorf("PrimaryResetAt = %v, want %v", usage.PrimaryResetAt, resetTime)
	}
	if usage.PrimaryWindowMinutes != 300 {
		t.Errorf("PrimaryWindowMinutes = %v, want 300", usage.PrimaryWindowMinutes)
	}
	if !usagePrimaryWindowAvailable(usage) {
		t.Errorf("usagePrimaryWindowAvailable should be true")
	}

	// Test recording tokens spent on an Antigravity account
	account := &Account{
		Type:     AccountTypeAntigravity,
		ID:       "test-anti-account",
		PlanType: "pro",
		Usage:    usage,
	}

	pool := newPoolState([]*Account{account}, false)
	handler := &proxyHandler{pool: pool, cfg: &config{}}

	ru := &RequestUsage{
		InputTokens:     500,
		OutputTokens:    120,
		ReasoningTokens: 30,
		BillableTokens:  620,
	}

	handler.recordAntigravityUsage(account, ru, "gemini-3.8-flash-high", "user-1", "origin-1", "req-1")

	// Verify account ID and plan type were attached
	if ru.AccountID != "test-anti-account" {
		t.Errorf("ru.AccountID = %q, want test-anti-account", ru.AccountID)
	}
	if ru.PlanType != "pro" {
		t.Errorf("ru.PlanType = %q, want pro", ru.PlanType)
	}

	// Verify token totals on the account were accumulated
	account.mu.Lock()
	tot := account.Totals
	account.mu.Unlock()

	if tot.TotalInputTokens != 500 {
		t.Errorf("TotalInputTokens = %d, want 500", tot.TotalInputTokens)
	}
	if tot.TotalOutputTokens != 120 {
		t.Errorf("TotalOutputTokens = %d, want 120", tot.TotalOutputTokens)
	}
	if tot.TotalReasoningTokens != 30 {
		t.Errorf("TotalReasoningTokens = %d, want 30", tot.TotalReasoningTokens)
	}
	if tot.TotalBillableTokens != 620 {
		t.Errorf("TotalBillableTokens = %d, want 620", tot.TotalBillableTokens)
	}
	if tot.RequestCount != 1 {
		t.Errorf("RequestCount = %d, want 1", tot.RequestCount)
	}

	// Verify pool stats reflects Antigravity accounts
	stats := pool.getPoolStats()
	if stats.AntigravityCount != 1 {
		t.Errorf("stats.AntigravityCount = %d, want 1", stats.AntigravityCount)
	}
	if len(stats.Accounts) != 1 {
		t.Fatalf("stats.Accounts count = %d, want 1", len(stats.Accounts))
	}
	brief := stats.Accounts[0]
	if brief.PrimaryPct != 25 {
		t.Errorf("brief.PrimaryPct = %d, want 25", brief.PrimaryPct)
	}
	if brief.PrimaryLabel != "5hr" {
		t.Errorf("brief.PrimaryLabel = %q, want 5hr", brief.PrimaryLabel)
	}
	if !brief.PrimaryAvailable {
		t.Errorf("brief.PrimaryAvailable should be true")
	}
}

func TestAntigravityAccountLoadInitializesUsage(t *testing.T) {
	resetTime := time.Now().Add(24 * time.Hour).UTC().Truncate(time.Second)
	rem := 1.0

	snapshot := AntigravityAccountSnapshot{
		FetchedAt: time.Now(),
		Models: map[string]AntigravityModelInfo{
			"gemini-3.8-flash-high": {
				ID: "gemini-3.8-flash-high",
				Quota: AntigravityQuotaInfo{
					RemainingFraction: &rem,
					ResetTime:         resetTime,
				},
			},
		},
	}

	auth := AntigravityAuthJSON{
		Type:          string(AccountTypeAntigravity),
		AccessToken:   "access",
		RefreshToken:  "refresh",
		ProjectID:     "project-1",
		PlanType:      "antigravity",
		ModelSnapshot: &snapshot,
	}

	data, err := json.Marshal(auth)
	if err != nil {
		t.Fatal(err)
	}

	provider := &AntigravityProvider{}
	account, err := provider.LoadAccount("test.json", "pool/test.json", data)
	if err != nil {
		t.Fatal(err)
	}
	if account == nil {
		t.Fatal("expected non-nil account")
	}

	if !usagePrimaryWindowAvailable(account.Usage) {
		t.Errorf("loaded account should have usagePrimaryWindowAvailable == true")
	}
	if account.Usage.PrimaryWindowMinutes != 300 {
		t.Errorf("account.Usage.PrimaryWindowMinutes = %d, want 300", account.Usage.PrimaryWindowMinutes)
	}
	if account.Usage.PrimaryResetAt != resetTime {
		t.Errorf("account.Usage.PrimaryResetAt = %v, want %v", account.Usage.PrimaryResetAt, resetTime)
	}
}

func TestAntigravityUsageUsesFiveHourAndWeeklyQuotaSummary(t *testing.T) {
	fetchedAt := time.Date(2026, 9, 21, 19, 32, 30, 0, time.UTC)
	body := []byte(`{
		"groups": [
			{
				"displayName": "Gemini Models",
				"buckets": [
					{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":0.3971,"resetTime":"2026-09-27T04:49:17Z"},
					{"bucketId":"gemini-5h","window":"5h","remainingFraction":0.3886,"resetTime":"2026-09-21T21:11:44Z"}
				]
			},
			{
				"displayName": "Claude and GPT models",
				"buckets": [
					{"bucketId":"3p-weekly","window":"weekly","remainingFraction":1,"resetTime":"2026-09-28T19:32:30Z"},
					{"bucketId":"3p-5h","window":"5h","remainingFraction":1,"resetTime":"2026-09-22T00:32:30Z"}
				]
			}
		]
	}`)
	summary, err := parseAntigravityQuotaSummary(body, fetchedAt)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := AntigravityAccountSnapshot{
		FetchedAt:    fetchedAt,
		Models:       map[string]AntigravityModelInfo{"gemini-3.8-flash-high": {ID: "gemini-3.8-flash-high"}},
		QuotaSummary: &summary,
	}
	usage := extractAntigravityAccountUsage(snapshot)

	if usage.PrimaryUsedPercent < 0.6113 || usage.PrimaryUsedPercent > 0.6115 {
		t.Errorf("PrimaryUsedPercent = %v, want 0.6114", usage.PrimaryUsedPercent)
	}
	if usage.SecondaryUsedPercent < 0.6028 || usage.SecondaryUsedPercent > 0.6030 {
		t.Errorf("SecondaryUsedPercent = %v, want 0.6029", usage.SecondaryUsedPercent)
	}
	if usage.PrimaryWindowMinutes != 300 || usage.SecondaryWindowMinutes != 10080 {
		t.Errorf("window minutes = %d/%d, want 300/10080", usage.PrimaryWindowMinutes, usage.SecondaryWindowMinutes)
	}
	if usage.PrimaryResetAt.Format(time.RFC3339) != "2026-09-21T21:11:44Z" {
		t.Errorf("PrimaryResetAt = %v", usage.PrimaryResetAt)
	}
	if usage.SecondaryResetAt.Format(time.RFC3339) != "2026-09-27T04:49:17Z" {
		t.Errorf("SecondaryResetAt = %v", usage.SecondaryResetAt)
	}
	if !usage.PrimaryUsageReported || !usage.SecondaryUsageReported {
		t.Errorf("summary windows should both be reported: %#v", usage)
	}
}
