package main

import (
	"testing"
	"time"
)

func TestModelDailyUsageUTCBoundsAndTotals(t *testing.T) {
	usage := testUsageStore(t)
	store := testDuckCosts(t, usage)
	now := time.Date(2026, 10, 1, 1, 0, 0, 0, time.FixedZone("east", 3*3600))
	for _, day := range []int{28, 29} {
		for _, id := range []string{"a", "b"} {
			for _, hour := range []int{0, 1} {
				recordTestCost(t, usage, RequestUsage{
					Timestamp: time.Date(2026, 9, day, hour, 0, 0, 0, time.UTC), AccountID: id,
					AccountType: AccountTypeKimi, Model: "model", InputTokens: 5, CachedInputTokens: 1,
					CacheCreationTokens: 2, OutputTokens: 2, ReasoningTokens: 1,
				}, 0.25)
			}
		}
	}
	for _, ts := range []string{"2026-09-30T00:00:00Z", "2026-09-30T23:59:59Z", "2026-10-01T00:00:00Z"} {
		at, err := time.Parse(time.RFC3339, ts)
		if err != nil {
			t.Fatal(err)
		}
		recordTestCost(t, usage, RequestUsage{
			Timestamp: at, AccountID: "a", AccountType: AccountTypeKimi, InputTokens: 5,
			CachedInputTokens: 1, CacheCreationTokens: 2, OutputTokens: 3, ReasoningTokens: 1,
		}, 0.25)
	}
	drainTestCosts(t, store)
	rows, err := store.getModelDailyUsageAt(1, now)
	if err != nil || len(rows) != 2 {
		t.Fatalf("rows: %+v, %v", rows, err)
	}
	want := []ModelDailyUsageEntry{
		{Date: "2026-09-29", AccountType: "kimi", Model: "model", InputTokens: 20, CachedTokens: 4, CacheCreationTokens: 8, OutputTokens: 8, ReasoningTokens: 4, RequestCount: 4, CostUSD: 1},
		{Date: "2026-09-30", AccountType: "kimi", InputTokens: 10, CachedTokens: 2, CacheCreationTokens: 4, OutputTokens: 6, ReasoningTokens: 2, RequestCount: 2, CostUSD: 0.5},
	}
	for i := range want {
		if rows[i] != want[i] {
			t.Fatalf("row %d: %+v", i, rows[i])
		}
	}
	if _, err := store.getModelDailyUsageAt(0, now); err == nil {
		t.Fatal("invalid window accepted")
	}
}
