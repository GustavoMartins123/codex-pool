package main

import (
	"database/sql"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func testDuckCosts(t testing.TB, store *usageStore) *DuckAnalytics {
	t.Helper()
	duck, err := newDuckAnalytics(filepath.Join(t.TempDir(), "usage.duckdb"), store.db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = duck.Close() })
	return duck
}

func recordTestCost(t testing.TB, store *usageStore, usage RequestUsage, cost float64) {
	t.Helper()
	if err := store.recordReliably(usage, cost); err != nil {
		t.Fatal(err)
	}
}

func drainTestCosts(t testing.TB, duck *DuckAnalytics) {
	t.Helper()
	if err := duck.drain(512); err != nil {
		t.Fatal(err)
	}
}

const legacyCostFixtureSchema = `
CREATE TABLE request_costs (
    timestamp TEXT, account_id TEXT, account_type TEXT, model TEXT,
    input_tokens INTEGER DEFAULT 0, cached_tokens INTEGER DEFAULT 0,
    output_tokens INTEGER DEFAULT 0, reasoning_tokens INTEGER DEFAULT 0, cost_usd REAL DEFAULT 0
);
CREATE TABLE daily_costs (
    date TEXT, account_id TEXT, account_type TEXT, model TEXT,
    input_tokens INTEGER DEFAULT 0, cached_tokens INTEGER DEFAULT 0,
    output_tokens INTEGER DEFAULT 0, reasoning_tokens INTEGER DEFAULT 0,
    request_count INTEGER DEFAULT 0, cost_usd REAL DEFAULT 0
);`

func TestDuckCostMigrationPreservesHistoryAndCutover(t *testing.T) {
	t.Setenv("DUCKDB_LOW_MEMORY", "1")
	dir := t.TempDir()
	boltPath := filepath.Join(dir, "proxy.db")
	duckPath := filepath.Join(dir, "usage.duckdb")
	legacyPath := filepath.Join(dir, "analytics.db")
	store, err := newUsageStore(boltPath, 30)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	today := time.Now().UTC().Truncate(24 * time.Hour)
	old := RequestUsage{
		Timestamp: today.Add(-12 * time.Hour), AccountID: "account", AccountType: AccountTypeCodex,
		UserID: "principal", Model: "gpt-5.5", InputTokens: 10, CachedInputTokens: 2, CacheCreationTokens: 1,
		OutputTokens: 3, ReasoningTokens: 2, BillableTokens: 12,
	}
	current := old
	current.Timestamp = today.Add(time.Minute)
	legacy, err := sql.Open("sqlite", legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	if _, err := legacy.Exec(legacyCostFixtureSchema + `
		ALTER TABLE request_costs ADD COLUMN cache_creation_tokens INTEGER DEFAULT 0;
		ALTER TABLE daily_costs ADD COLUMN cache_creation_tokens INTEGER DEFAULT 0;
	`); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`INSERT INTO daily_costs VALUES (?, ?, ?, ?, 10, 2, 3, 2, 1, 1.25, 1)`,
		old.Timestamp.Format(time.DateOnly), old.AccountID, string(old.AccountType), old.Model); err != nil {
		t.Fatal(err)
	}
	if _, err := legacy.Exec(`INSERT INTO request_costs VALUES (?, ?, ?, ?, 10, 2, 3, 2, 2.0, 1)`,
		current.Timestamp.Format(time.RFC3339Nano), current.AccountID, string(current.AccountType), current.Model); err != nil {
		t.Fatal(err)
	}
	if err := legacy.Close(); err != nil {
		t.Fatal(err)
	}
	sourceBefore, err := os.ReadFile(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	// The same current request was already written to the durable outbox.
	recordTestCost(t, store, current, 2)
	// Events can have DuckDB history exclusive to any one grouping dimension.
	unmatchedAccount := current
	unmatchedAccount.AccountID = "duck-only"
	unmatchedAccount.UserID = ""
	recordTestCost(t, store, unmatchedAccount, 0.5)
	unmatchedModel := current
	unmatchedModel.Model = "gpt-5.6-sol"
	unmatchedModel.RequestID = "model"
	recordTestCost(t, store, unmatchedModel, 0.125)
	unmatchedProvider := current
	unmatchedProvider.AccountType = AccountTypeClaude
	unmatchedProvider.RequestID = "provider"
	recordTestCost(t, store, unmatchedProvider, 0.25)
	unmatchedDay := old
	unmatchedDay.Timestamp = old.Timestamp.AddDate(0, 0, -1)
	recordTestCost(t, store, unmatchedDay, 0.375)

	duck, err := newDuckAnalytics(duckPath, store.db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = duck.Close() })
	if err := duck.migrateSQLiteCosts(t.Context(), boltPath, filepath.Join(dir, "missing.db"), today); err == nil {
		t.Fatal("expected a missing SQLite source to fail before writing the boundary")
	}
	if err := duck.migrateSQLiteCosts(t.Context(), boltPath, "", today); err != nil {
		t.Fatal(err)
	}
	sourceAfter, err := os.ReadFile(legacyPath)
	if err != nil || !reflect.DeepEqual(sourceAfter, sourceBefore) {
		t.Fatalf("SQLite import modified the source: %v", err)
	}
	drainTestCosts(t, duck)

	wantModels := []ModelDailyUsageEntry{
		{Date: unmatchedDay.Timestamp.Format(time.DateOnly), AccountType: "codex", Model: old.Model, InputTokens: 10, CachedTokens: 2, CacheCreationTokens: 1, OutputTokens: 3, ReasoningTokens: 2, RequestCount: 1, CostUSD: 0.375},
		{Date: old.Timestamp.Format(time.DateOnly), AccountType: "codex", Model: old.Model, InputTokens: 10, CachedTokens: 2, CacheCreationTokens: 1, OutputTokens: 3, ReasoningTokens: 2, RequestCount: 1, CostUSD: 1.25},
		{Date: today.Format(time.DateOnly), AccountType: "claude", Model: old.Model, InputTokens: 10, CachedTokens: 2, CacheCreationTokens: 1, OutputTokens: 3, ReasoningTokens: 2, RequestCount: 1, CostUSD: 0.25},
		{Date: today.Format(time.DateOnly), AccountType: "codex", Model: old.Model, InputTokens: 20, CachedTokens: 4, CacheCreationTokens: 2, OutputTokens: 6, ReasoningTokens: 4, RequestCount: 2, CostUSD: 2.5},
		{Date: today.Format(time.DateOnly), AccountType: "codex", Model: unmatchedModel.Model, InputTokens: 10, CachedTokens: 2, CacheCreationTokens: 1, OutputTokens: 3, ReasoningTokens: 2, RequestCount: 1, CostUSD: 0.125},
	}
	gotModels, err := duck.getModelDailyUsageAt(42, today)
	if err != nil || !reflect.DeepEqual(gotModels, wantModels) {
		t.Fatalf("model history = %+v (%v), want %+v", gotModels, err, wantModels)
	}
	wantDaily := []DailyCostEntry{
		{Date: unmatchedDay.Timestamp.Format(time.DateOnly), AccountType: "codex", RequestCount: 1, CostUSD: 0.375},
		{Date: old.Timestamp.Format(time.DateOnly), AccountType: "codex", RequestCount: 1, CostUSD: 1.25},
	}
	gotDaily, err := duck.getDailyCosts(30)
	if err != nil || !reflect.DeepEqual(gotDaily, wantDaily) {
		t.Fatalf("daily costs = %+v (%v), want %+v", gotDaily, err, wantDaily)
	}
	wantStats := map[string]AccountCostStats{
		"account":   {CostUSD: 4, FirstSeen: today.AddDate(0, 0, -2)},
		"duck-only": {CostUSD: 0.5, FirstSeen: current.Timestamp},
	}
	gotStats, err := duck.getAllTimeAccountCostStats()
	if err != nil || !reflect.DeepEqual(gotStats, wantStats) {
		t.Fatalf("account costs = %+v (%v), want %+v", gotStats, err, wantStats)
	}
	economics, err := duck.getSignalEconomics([]string{"account", "other"}, today)
	if err != nil || len(economics.dailyCosts) != 4 ||
		!economics.firstSeen["account"].Equal(today.AddDate(0, 0, -2)) {
		t.Fatalf("economics = %+v, err = %v", economics, err)
	}

	// A fact recorded after the copy can describe an earlier observation.
	// Membership in the new history follows its durable delivery order.
	recordTestCost(t, store, old, 0.75)
	drainTestCosts(t, duck)
	assertCost := func(reader costAnalyticsRepository) {
		t.Helper()
		costs, err := reader.getCostByAccount(30)
		if err != nil || costs["account"] != 4.75 || costs["duck-only"] != 0.5 {
			t.Fatalf("cost after cutover = %+v, err = %v", costs, err)
		}
	}
	assertCost(duck)
	if err := duck.migrateSQLiteCosts(t.Context(), boltPath, filepath.Join(dir, "missing.db"), today); err != nil {
		t.Fatal(err)
	}
	assertCost(duck)

	if err := duck.Close(); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	manifest, err := createPairedBackup(boltPath, duckPath, filepath.Join(dir, "backups"))
	if err != nil {
		t.Fatal(err)
	}
	target := t.TempDir()
	boltRestored, duckRestored := filepath.Join(target, "proxy.db"), filepath.Join(target, "usage.duckdb")
	if err := restorePairedBackup(manifest, boltRestored, duckRestored); err != nil {
		t.Fatal(err)
	}
	restoredStore, err := newUsageStore(boltRestored, 30)
	if err != nil {
		t.Fatal(err)
	}
	defer restoredStore.Close()
	restored, err := newDuckAnalytics(duckRestored, restoredStore.db)
	if err != nil {
		t.Fatal(err)
	}
	defer restored.Close()
	assertCost(restored)
	if err := restored.DeleteAccountUsage("account"); err != nil {
		t.Fatal(err)
	}
	costs, err := restored.getCostByAccount(30)
	if err != nil || len(costs) != 1 || costs["duck-only"] != 0.5 {
		t.Fatalf("deleted account cost remains in restored analytics: %+v, %v", costs, err)
	}
}

func TestDuckCostMigrationRemainsRetryableAfterInvalidHistory(t *testing.T) {
	store := testUsageStore(t)
	duck := testDuckCosts(t, store)
	dir := t.TempDir()
	storePath := filepath.Join(dir, "proxy.db")
	legacyPath := filepath.Join(dir, "old costs.db")
	legacy, err := sql.Open("sqlite", legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	defer legacy.Close()
	if _, err := legacy.Exec(legacyCostFixtureSchema); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC().Truncate(24 * time.Hour)
	day := now.AddDate(0, 0, -1).Format(time.DateOnly)
	if _, err := legacy.Exec(`
		INSERT INTO daily_costs (date, account_id, account_type, model, input_tokens, request_count, cost_usd)
		VALUES (?, 'a', 'codex', 'model', 10, 1, 1.5), ('invalid', 'b', 'codex', 'model', 20, 1, 2.5)
	`, day); err != nil {
		t.Fatal(err)
	}
	if err := duck.migrateSQLiteCosts(t.Context(), storePath, legacyPath, now); err == nil {
		t.Fatal("migration accepted an invalid day")
	}
	costs, err := duck.getCostByAccount(30)
	if err != nil || len(costs) != 0 {
		t.Fatalf("failed import left partial cost history: %v, %v", costs, err)
	}
	if _, err := legacy.Exec(`DELETE FROM daily_costs WHERE date = 'invalid'`); err != nil {
		t.Fatal(err)
	}
	if err := duck.migrateSQLiteCosts(t.Context(), storePath, legacyPath, now); err != nil {
		t.Fatal(err)
	}
	models, err := duck.getModelDailyUsageAt(30, now)
	if err != nil || len(models) != 1 || models[0].CostUSD != 1.5 || models[0].CacheCreationTokens != 0 {
		t.Fatalf("retry with pre-cache-creation schema: %+v, %v", models, err)
	}
}
