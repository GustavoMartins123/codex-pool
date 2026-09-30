package main

import (
	"encoding/json"
	"math"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"
)

func TestSignalAnalyticsEmptyStoresReturnArrays(t *testing.T) {
	usage, err := newUsageStore(filepath.Join(t.TempDir(), "usage.db"), 30)
	if err != nil {
		t.Fatal(err)
	}
	defer usage.Close()
	analytics, err := newAnalyticsStore(filepath.Join(t.TempDir(), "analytics.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer analytics.Close()
	h := &proxyHandler{pool: newPoolState(nil, false), store: usage, analyticsStore: analytics}
	recorder := httptest.NewRecorder()
	h.handleSignalAnalytics(recorder, httptest.NewRequest("GET", "/api/pool/signal", nil))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response map[string]any
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	for _, field := range []string{"economics", "hourly", "origin_weekly", "model_daily", "quota_capacity", "model_efficiency", "reset_observations"} {
		rows, ok := response[field].([]any)
		if !ok || len(rows) != 0 {
			t.Errorf("%s=%v: expected an empty array", field, response[field])
		}
	}
}

func TestSignalEconomicsPreservesMembershipBillingAndLiveCosts(t *testing.T) {
	store, err := newAnalyticsStore(filepath.Join(t.TempDir(), "analytics.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	for _, row := range []struct {
		day     int
		account string
		model   string
		cost    float64
	}{
		{-45, "current", "model-a", 100}, {-45, "current", "model-b", 50},
		{-1, "current", "model-a", 20}, {0, "current", "model-a", 2},
		{-60, "removed", "model-a", 900},
	} {
		if _, err := store.db.Exec(`INSERT INTO daily_costs
			(date, account_id, account_type, model, cost_usd)
			VALUES (?, ?, ?, ?, ?)`, now.AddDate(0, 0, row.day).Format("2006-01-02"), row.account,
			string(AccountTypeCodex), row.model, row.cost); err != nil {
			t.Fatal(err)
		}
	}
	for _, row := range []struct {
		at       time.Time
		account  string
		provider AccountType
		cost     float64
	}{
		{now.Add(-time.Hour), "current", AccountTypeCodex, 5},
		{now.Add(-time.Hour), "live", AccountTypeClaude, 3},
		{now.AddDate(0, 0, -1), "current", AccountTypeCodex, 999},
	} {
		if err := store.recordRequest(RequestUsage{Timestamp: row.at, AccountID: row.account, AccountType: row.provider}, row.cost); err != nil {
			t.Fatal(err)
		}
	}
	current := &Account{ID: "current", Type: AccountTypeCodex, PlanType: "pro"}
	live := &Account{ID: "live", Type: AccountTypeClaude, PlanType: "pro"}
	unmeasured := &Account{ID: "unmeasured", Type: AccountTypeCodex, PlanType: "plus"}
	h := &proxyHandler{pool: newPoolState([]*Account{current, live, unmeasured}, false), analyticsStore: store}
	points, err := h.buildSignalEconomics(now)
	if err != nil {
		t.Fatal(err)
	}
	if len(points) != 46 {
		t.Fatalf("days=%d, want 46", len(points))
	}
	for index, point := range points {
		if want := now.AddDate(0, 0, index-45).Format("2006-01-02"); point.Date != want {
			t.Fatalf("date=%s, want %s", point.Date, want)
		}
	}
	if points[0].DailyAPIValue != 150 || points[1].DailyAPIValue != 0 || points[29].CumulativeSubscriptionSpend != 240 || points[30].CumulativeSubscriptionSpend != 440 {
		t.Fatalf("unexpected initial values or billing boundary: first=%+v, day29=%+v, day30=%+v", points[0], points[29], points[30])
	}
	latest := points[len(points)-1]
	if latest.DailyAPIValue != 10 || latest.CumulativeAPIValue != 180 || latest.ProviderAPIValue["codex"] != 7 || latest.ProviderAPIValue["claude"] != 3 {
		t.Fatalf("live or historical costs double-counted: %+v", latest)
	}
	h.pool = newPoolState([]*Account{live, unmeasured}, false)
	points, err = h.buildSignalEconomics(now)
	if err != nil || len(points) != 1 || points[0].CumulativeAPIValue != 3 || points[0].CumulativeSubscriptionSpend != 40 {
		t.Fatalf("removed membership retained: points=%+v, err=%v", points, err)
	}
	h.pool = newPoolState([]*Account{current, live, unmeasured, {ID: "removed", Type: AccountTypeCodex, PlanType: "plus"}}, false)
	points, err = h.buildSignalEconomics(now)
	if err != nil || len(points) != 61 || points[len(points)-1].CumulativeAPIValue != 1080 || points[len(points)-1].CumulativeSubscriptionSpend != 500 {
		t.Fatalf("re-added history or billing lost: points=%+v, err=%v", points, err)
	}
}

func TestSignalEconomicsUsesOneUTCDayAndExactLiveMeasurementTime(t *testing.T) {
	store, err := newAnalyticsStore(filepath.Join(t.TempDir(), "analytics.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	store.db.SetMaxOpenConns(1)
	now := time.Date(2026, 8, 1, 21, 30, 0, 0, time.FixedZone("local", -3*60*60))
	first := time.Date(2026, 8, 2, 0, 1, 2, 0, time.UTC)
	for _, at := range []time.Time{first, first.Add(-2 * time.Minute), first.Add(time.Minute), first.AddDate(0, 0, 1)} {
		if err := store.recordRequest(RequestUsage{Timestamp: at, AccountID: "a'\"", AccountType: AccountTypeCodex}, 1); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := store.getSignalEconomics([]string{"a'\""}, now)
	if err != nil {
		t.Fatal(err)
	}
	if !snapshot.firstSeen["a'\""].Equal(first) || len(snapshot.dailyCosts) != 1 || snapshot.dailyCosts[0].Date != "2026-08-02" || snapshot.dailyCosts[0].CostUSD != 2 {
		t.Fatalf("UTC snapshot=%+v", snapshot)
	}
	h := &proxyHandler{pool: newPoolState([]*Account{{ID: "a'\"", Type: AccountTypeCodex, PlanType: "pro"}}, false), analyticsStore: store}
	points, err := h.buildSignalEconomics(now)
	if err != nil || len(points) != 1 || points[0].Date != "2026-08-02" || points[0].CumulativeAPIValue != 2 {
		t.Fatalf("UTC timeline=%+v, err=%v", points, err)
	}
}

func TestSignalEconomicsRejectsQueryAndDateErrors(t *testing.T) {
	for _, failure := range []string{"closed", "missing live table", "invalid historical date", "invalid live timestamp"} {
		t.Run(failure, func(t *testing.T) {
			store, err := newAnalyticsStore(filepath.Join(t.TempDir(), "analytics.db"))
			if err != nil {
				t.Fatal(err)
			}
			defer store.Close()
			var mutation string
			switch failure {
			case "closed":
				err = store.db.Close()
			case "missing live table":
				mutation = `DROP TABLE request_costs`
			case "invalid historical date":
				mutation = `INSERT INTO daily_costs (date, account_id, account_type, cost_usd) VALUES ('invalid', 'current', 'codex', 1)`
			case "invalid live timestamp":
				mutation = `INSERT INTO request_costs (timestamp, account_id, account_type, cost_usd) VALUES ('` + time.Now().UTC().Format("2006-01-02") + `T12:invalid', 'current', 'codex', 1)`
			}
			if mutation != "" {
				_, err = store.db.Exec(mutation)
			}
			if err != nil {
				t.Fatal(err)
			}
			h := &proxyHandler{pool: newPoolState([]*Account{{ID: "current", Type: AccountTypeCodex, PlanType: "pro"}}, false), analyticsStore: store}
			points, err := h.buildSignalEconomics(time.Now())
			if err == nil || points != nil {
				t.Fatalf("partial economics accepted: points=%+v, err=%v", points, err)
			}
			recorder := httptest.NewRecorder()
			h.handleSignalAnalytics(recorder, httptest.NewRequest(http.MethodGet, "/api/pool/signal", nil))
			if recorder.Code != http.StatusInternalServerError {
				t.Fatalf("status=%d, want 500", recorder.Code)
			}
		})
	}
}

func TestSignalEconomicsAggregatesProviderAndMultipleModels(t *testing.T) {
	store, err := newAnalyticsStore(filepath.Join(t.TempDir(), "analytics.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer store.Close()
	now := time.Date(2026, 8, 1, 12, 0, 0, 0, time.UTC)
	for _, row := range []struct{ account, provider, model string }{
		{"a", "claude", "m1"}, {"a", "codex", "m2"}, {"b", "codex", "m1"}, {"c", "claude", "m1"},
	} {
		if _, err := store.db.Exec(`INSERT INTO daily_costs (date, account_id, account_type, model, cost_usd)
			VALUES ('2026-07-31', ?, ?, ?, 0.1)`, row.account, row.provider, row.model); err != nil {
			t.Fatal(err)
		}
	}
	snapshot, err := store.getSignalEconomics([]string{"a", "b", "c"}, now)
	if err != nil {
		t.Fatal(err)
	}
	if len(snapshot.dailyCosts) != 2 || len(snapshot.firstSeen) != 3 {
		t.Fatalf("unexpected aggregate grain: %+v", snapshot)
	}
	for _, row := range snapshot.dailyCosts {
		want := 0.1
		if row.AccountType == "codex" {
			want = 0.3
		}
		if row.Date != "2026-07-31" || math.Abs(row.CostUSD-want) > 1e-9 {
			t.Fatalf("provider attribution changed: %+v", row)
		}
	}
}

func TestSignalAnalyticsLinksWeeklyOriginDrainAndCurrentAccountEconomics(t *testing.T) {
	usage, err := newUsageStore(filepath.Join(t.TempDir(), "usage.db"), 30)
	if err != nil {
		t.Fatal(err)
	}
	defer usage.Close()

	now := time.Now().UTC()
	if err := usage.record(RequestUsage{
		Timestamp:         now,
		AccountID:         "current",
		AccountType:       AccountTypeCodex,
		OriginID:          "origin-hash",
		InputTokens:       100,
		CachedInputTokens: 60,
		OutputTokens:      20,
		ReasoningTokens:   5,
		BillableTokens:    65,
	}); err != nil {
		t.Fatal(err)
	}

	analytics, err := newAnalyticsStore(filepath.Join(t.TempDir(), "analytics.db"))
	if err != nil {
		t.Fatal(err)
	}
	defer analytics.Close()
	firstDate := now.AddDate(0, 0, -10).Format("2006-01-02")
	for _, row := range []struct {
		account string
		cost    float64
	}{
		{account: "current", cost: 100},
		{account: "removed", cost: 900},
	} {
		if _, err := analytics.db.Exec(`
			INSERT INTO daily_costs (date, account_id, account_type, model, request_count, cost_usd)
			VALUES (?, ?, ?, ?, ?, ?)`, firstDate, row.account, string(AccountTypeCodex), "gpt-5.6-sol", 1, row.cost); err != nil {
			t.Fatal(err)
		}
	}

	h := &proxyHandler{
		pool:           newPoolState([]*Account{{ID: "current", Type: AccountTypeCodex, PlanType: "pro"}}, false),
		store:          usage,
		analyticsStore: analytics,
	}
	recorder := httptest.NewRecorder()
	h.handleSignalAnalytics(recorder, httptest.NewRequest("GET", "/api/pool/signal?weeks=2", nil))

	var response SignalAnalyticsResponse
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.OriginWeekly) != 1 {
		t.Fatalf("origin weekly rows = %d, want 1", len(response.OriginWeekly))
	}
	origin := response.OriginWeekly[0]
	if origin.OriginID != "origin-hash" || origin.AccountID != hashAccountID("current") || origin.BillableTokens != 65 {
		t.Fatalf("origin weekly row = %+v", origin)
	}
	if len(response.Economics) == 0 {
		t.Fatal("economics timeline is empty")
	}
	latest := response.Economics[len(response.Economics)-1]
	if latest.CumulativeAPIValue != 100 {
		t.Fatalf("cumulative API value = %v, want current-account value 100", latest.CumulativeAPIValue)
	}
	if latest.CumulativeSubscriptionSpend != 200 {
		t.Fatalf("cumulative subscription spend = %v, want 200", latest.CumulativeSubscriptionSpend)
	}
}
