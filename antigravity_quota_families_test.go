package main

import (
	"encoding/json"
	"net/http/httptest"
	"testing"
	"time"
)

func familyByID(t *testing.T, families []AccountQuotaFamily, id string) AccountQuotaFamily {
	t.Helper()
	for _, family := range families {
		if family.ID == id {
			return family
		}
	}
	t.Fatalf("family %q missing from %+v", id, families)
	return AccountQuotaFamily{}
}

func TestAntigravityQuotaFamiliesSeparateGeminiFromThirdParty(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	full, spent := 1.0, 0.0
	summary := &AntigravityQuotaSummary{Groups: []AntigravityQuotaGroup{
		{DisplayName: "Claude and GPT models", Buckets: []AntigravityQuotaBucket{
			{BucketID: "3p-weekly", Window: "weekly", RemainingFraction: &spent, ResetTime: now.Add(5*24*time.Hour + 12*time.Hour)},
			{BucketID: "3p-5h", Window: "5h", RemainingFraction: &full, ResetTime: now.Add(5 * time.Hour)},
		}},
		{DisplayName: "Gemini Models", Buckets: []AntigravityQuotaBucket{
			{BucketID: "gemini-weekly", Window: "weekly", RemainingFraction: &full, ResetTime: now.Add(6 * 24 * time.Hour)},
			{BucketID: "gemini-5h", Window: "5h", RemainingFraction: &full, ResetTime: now.Add(4 * time.Hour)},
		}},
	}}

	families := antigravityQuotaFamilies(summary, now)
	if len(families) != 2 || families[0].ID != "gemini" || families[1].ID != "3p" {
		t.Fatalf("families must list Gemini first: %+v", families)
	}
	gemini, thirdParty := familyByID(t, families, "gemini"), familyByID(t, families, "3p")
	if gemini.Weekly.UsedPct != 0 || gemini.Weekly.ResetMinutes != 6*24*60 || gemini.Weekly.WindowMinutes != 7*24*60 {
		t.Fatalf("gemini weekly = %+v", gemini.Weekly)
	}
	if thirdParty.Weekly.UsedPct != 100 || thirdParty.FiveHour.UsedPct != 0 {
		t.Fatalf("third-party windows = %+v / %+v", thirdParty.Weekly, thirdParty.FiveHour)
	}

	// The blended account window keeps reading the most consumed bucket, which
	// routing and scoring still rely on.
	remaining, _ := antigravitySummaryWindow(summary, "weekly")
	if remaining == nil || *remaining != 0 {
		t.Fatalf("blended weekly remaining = %v, want 0", remaining)
	}
}

func TestAntigravityQuotaFamiliesTreatPassedResetAsRefilled(t *testing.T) {
	now := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	spent := 0.0
	summary := &AntigravityQuotaSummary{Buckets: []AntigravityQuotaBucket{
		{BucketID: "gemini-weekly", Window: "weekly", RemainingFraction: &spent, ResetTime: now.Add(-time.Minute)},
	}}
	families := antigravityQuotaFamilies(summary, now)
	if len(families) != 1 || families[0].Weekly == nil || families[0].Weekly.UsedPct != 0 || families[0].Weekly.ResetMinutes != 0 {
		t.Fatalf("passed reset = %+v", families)
	}
	if families[0].FiveHour != nil {
		t.Fatalf("missing 5h bucket must stay unreported: %+v", families[0].FiveHour)
	}
}

func TestAntigravityQuotaFamiliesIgnoreMissingSummary(t *testing.T) {
	if families := antigravityQuotaFamilies(nil, time.Now()); families != nil {
		t.Fatalf("nil summary = %+v", families)
	}
	unknown := &AntigravityQuotaSummary{Buckets: []AntigravityQuotaBucket{{BucketID: "other-weekly", Window: "weekly"}}}
	if families := antigravityQuotaFamilies(unknown, time.Now()); len(families) != 0 {
		t.Fatalf("unknown family = %+v", families)
	}
}

func TestPoolStatsExposesAntigravityQuotaFamilies(t *testing.T) {
	account := &Account{Type: AccountTypeAntigravity, ID: "families"}
	pool := newPoolState([]*Account{account}, false)
	full, spent := 1.0, 0.0
	reset := time.Now().Add(48 * time.Hour)
	pool.antigravity.models.ReplaceAccount(account.ID, AntigravityAccountSnapshot{
		FetchedAt: time.Now(),
		Models:    map[string]AntigravityModelInfo{"gemini-3.8-flash-high": {ID: "gemini-3.8-flash-high"}},
		QuotaSummary: &AntigravityQuotaSummary{Buckets: []AntigravityQuotaBucket{
			{BucketID: "gemini-weekly", Window: "weekly", RemainingFraction: &full, ResetTime: reset},
			{BucketID: "3p-weekly", Window: "weekly", RemainingFraction: &spent, ResetTime: reset},
		}},
	})
	handler := &proxyHandler{pool: pool}
	recorder := httptest.NewRecorder()
	handler.handlePoolStats(recorder, httptest.NewRequest("GET", "/api/pool/stats", nil))

	var payload PoolStats
	if err := json.Unmarshal(recorder.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	got := payload.Accounts[0]
	if len(got.QuotaFamilies) != 2 || got.QuotaFamilies[0].Weekly.UsedPct != 0 || got.QuotaFamilies[1].Weekly.UsedPct != 100 {
		t.Fatalf("quota families = %+v", got.QuotaFamilies)
	}
}
