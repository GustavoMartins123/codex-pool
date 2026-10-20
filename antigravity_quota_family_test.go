package main

import (
	"testing"
	"time"
)

// The Gemini weekly bucket omits remainingFraction: proto3 drops zero values.
const antigravityExhaustedGeminiQuotaFixture = `{"groups":[
 {"displayName":"Gemini Models","buckets":[
  {"bucketId":"gemini-weekly","window":"weekly","resetTime":"2026-10-01T10:40:06Z"},
  {"bucketId":"gemini-5h","window":"5h","resetTime":"2026-09-28T23:02:27Z","remainingFraction":0.9998}]},
 {"displayName":"Claude and GPT models","buckets":[
  {"bucketId":"3p-weekly","window":"weekly","resetTime":"2026-10-05T18:02:13Z","remainingFraction":0.75},
  {"bucketId":"3p-5h","window":"5h","resetTime":"2026-09-28T23:02:13Z","remainingFraction":0.5}]}]}`

func TestAntigravityQuotaSummaryTreatsMissingFractionAsExhausted(t *testing.T) {
	now := time.Date(2026, 9, 28, 18, 0, 0, 0, time.UTC)
	summary, err := parseAntigravityQuotaSummary([]byte(antigravityExhaustedGeminiQuotaFixture), now)
	if err != nil {
		t.Fatal(err)
	}
	weekly := summary.Groups[0].Buckets[0]
	if weekly.RemainingFraction == nil || *weekly.RemainingFraction != 0 {
		t.Fatalf("missing fraction decoded as %v, want exhausted", weekly.RemainingFraction)
	}
	weeklyReset := time.Date(2026, 10, 1, 10, 40, 6, 0, time.UTC)
	if got := summary.exhaustedUntil("gemini-3.8-flash-high", now); !got.Equal(weeklyReset) {
		t.Fatalf("gemini exhausted until %s, want weekly reset %s", got, weeklyReset)
	}
	if got := summary.exhaustedUntil("claude-opus-4-6-thinking", now); !got.IsZero() {
		t.Fatalf("claude must not inherit the gemini exhaustion, got %s", got)
	}
	if got := summary.exhaustedUntil("gemini-3.1-flash-image", now); !got.IsZero() {
		t.Fatalf("image models have no shared family bucket, got %s", got)
	}
}

func TestAntigravityModelQuotaInfoWithoutFractionIsExhausted(t *testing.T) {
	snapshot, err := parseAntigravityModelSnapshot([]byte(`{"models":{"gemini-live":{"displayName":"Gemini Live","quotaInfo":{"resetTime":"2099-01-01T00:00:00Z"}},"gemini-fresh":{"displayName":"Gemini Fresh","quotaInfo":{}}}}`), time.Now())
	if err != nil {
		t.Fatal(err)
	}
	if fraction := snapshot.Models["gemini-live"].Quota.RemainingFraction; fraction == nil || *fraction != 0 {
		t.Fatalf("reset without fraction decoded as %v, want exhausted", fraction)
	}
	if fraction := snapshot.Models["gemini-fresh"].Quota.RemainingFraction; fraction != nil {
		t.Fatalf("quota without reset must stay unknown, got %v", *fraction)
	}
}

func TestAntigravityFamilyQuotaGatesOnlyItsOwnModels(t *testing.T) {
	antigravityModels.Reset()
	t.Cleanup(antigravityModels.Reset)
	now := time.Now().UTC()
	exhausted, half, full := 0.0, 0.5, 1.0
	reset := now.Add(48 * time.Hour)
	summary := AntigravityQuotaSummary{FetchedAt: now, Groups: []AntigravityQuotaGroup{
		{DisplayName: "Gemini Models", Buckets: []AntigravityQuotaBucket{
			{BucketID: "gemini-weekly", Window: "weekly", RemainingFraction: &exhausted, ResetTime: reset},
			{BucketID: "gemini-5h", Window: "5h", RemainingFraction: &full, ResetTime: now.Add(time.Hour)},
		}},
		{DisplayName: "Claude and GPT models", Buckets: []AntigravityQuotaBucket{
			{BucketID: "3p-weekly", Window: "weekly", RemainingFraction: &half, ResetTime: reset},
			{BucketID: "3p-5h", Window: "5h", RemainingFraction: &half, ResetTime: now.Add(time.Hour)},
		}},
	}}
	snapshot := AntigravityAccountSnapshot{
		FetchedAt: now,
		Models: map[string]AntigravityModelInfo{
			"gemini-3.8-flash-high":    {ID: "gemini-3.8-flash-high", Quota: AntigravityQuotaInfo{RemainingFraction: &full}},
			"claude-opus-4-6-thinking": {ID: "claude-opus-4-6-thinking", Quota: AntigravityQuotaInfo{RemainingFraction: &full}},
		},
		QuotaSummary: &summary,
	}
	account := &Account{Type: AccountTypeAntigravity, ID: "ag", ModelRateLimits: map[string]time.Time{}, Usage: extractAntigravityAccountUsage(snapshot)}
	antigravityModels.ReplaceAccount(account.ID, snapshot)

	if available, until := antigravityModels.DiscoveryAvailability(account.ID, "gemini-3.8-flash-high", now); available || !until.Equal(reset) {
		t.Fatalf("gemini availability = %v until %s despite per-model fraction 1", available, until)
	}
	if available, _ := antigravityModels.DiscoveryAvailability(account.ID, "claude-opus-4-6-thinking", now); !available {
		t.Fatal("claude family still has quota")
	}
	account.mu.Lock()
	usageExhausted := accountUsageExhaustedLocked(account)
	account.mu.Unlock()
	if usageExhausted {
		t.Fatal("one exhausted family must not exhaust the whole account")
	}

	pool := newPoolState([]*Account{account}, false)
	if got := pool.smartCandidateForModelForUser("", "", nil, AccountTypeAntigravity, "", "", "claude-opus-4-6-thinking", RoutingBalanced); got.Account != account {
		t.Fatalf("claude request was not routed to the account with claude quota: %#v", got)
	}
	if got := pool.smartCandidateForModelForUser("", "", nil, AccountTypeAntigravity, "", "", "gemini-3.8-flash-high", RoutingBalanced); got.Account != nil {
		t.Fatalf("gemini request routed to an account with an exhausted gemini week: %s", got.Account.ID)
	}

	summary.Groups[1].Buckets[0].RemainingFraction = &exhausted
	account.mu.Lock()
	usageExhausted = accountUsageExhaustedLocked(account)
	account.mu.Unlock()
	if !usageExhausted {
		t.Fatal("an account with every family exhausted must report exhausted usage")
	}
}
