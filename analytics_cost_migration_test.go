package main

import (
	"path/filepath"
	"reflect"
	"testing"
	"time"
)

func TestDuckCostMigrationPreservesHistoryAndCutover(t *testing.T) {
	t.Setenv("DUCKDB_LOW_MEMORY", "1")
	dir := t.TempDir()
	boltPath := filepath.Join(dir, "proxy.db")
	duckPath := filepath.Join(dir, "usage.duckdb")
	legacyPath := filepath.Join(dir, "SQLite history.db")
	store, err := newUsageStore(boltPath, 30)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })

	legacy, err := newAnalyticsStore(legacyPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = legacy.Close() })

	today := time.Now().UTC().Truncate(24 * time.Hour)
	old := RequestUsage{
		Timestamp: today.Add(-12 * time.Hour), AccountID: "account", AccountType: AccountTypeCodex,
		UserID: "principal", Model: "gpt-5.5", InputTokens: 10, CachedInputTokens: 2, CacheCreationTokens: 1,
		OutputTokens: 3, ReasoningTokens: 2, BillableTokens: 12,
	}
	current := old
	current.Timestamp = today.Add(time.Minute)
	if err := legacy.recordRequest(old, 1.25); err != nil {
		t.Fatal(err)
	}
	legacy.runDailyRollup()
	if err := legacy.recordRequest(current, 2.0); err != nil {
		t.Fatal(err)
	}
	// The same current request was already written to the durable outbox.
	if err := store.recordReliably(current, 2.0); err != nil {
		t.Fatal(err)
	}

	duck, err := newDuckAnalytics(duckPath, store.db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = duck.Close() })
	if err := duck.migrateSQLiteCosts(t.Context(), filepath.Join(dir, "missing.db"), today); err == nil {
		t.Fatal("expected a missing SQLite source to fail before writing the boundary")
	}
	if err := duck.migrateSQLiteCosts(t.Context(), legacyPath, today); err != nil {
		t.Fatal(err)
	}
	if err := duck.drain(100); err != nil {
		t.Fatal(err)
	}

	wantModels, err := legacy.getModelDailyUsageAt(42, today)
	if err != nil {
		t.Fatal(err)
	}
	gotModels, err := duck.getModelDailyUsageAt(42, today)
	if err != nil || !reflect.DeepEqual(gotModels, wantModels) {
		t.Fatalf("model history = %+v (%v), want %+v", gotModels, err, wantModels)
	}
	wantDaily, err := legacy.getDailyCosts(30)
	if err != nil {
		t.Fatal(err)
	}
	gotDaily, err := duck.getDailyCosts(30)
	if err != nil || !reflect.DeepEqual(gotDaily, wantDaily) {
		t.Fatalf("daily costs = %+v (%v), want %+v", gotDaily, err, wantDaily)
	}
	wantStats, err := legacy.getAllTimeAccountCostStats()
	if err != nil {
		t.Fatal(err)
	}
	gotStats, err := duck.getAllTimeAccountCostStats()
	if err != nil || !reflect.DeepEqual(gotStats, wantStats) {
		t.Fatalf("account costs = %+v (%v), want %+v", gotStats, err, wantStats)
	}
	economics, err := duck.getSignalEconomics([]string{"account", "other"}, today)
	if err != nil || len(economics.dailyCosts) != 2 ||
		!economics.firstSeen["account"].Equal(today.AddDate(0, 0, -1)) {
		t.Fatalf("economics = %+v, err = %v", economics, err)
	}

	// A fact recorded after the copy can describe an earlier observation.
	// Membership in the new history follows its durable delivery order.
	if err := store.recordReliably(old, 0.75); err != nil {
		t.Fatal(err)
	}
	if err := duck.drain(100); err != nil {
		t.Fatal(err)
	}
	assertCost := func(reader costAnalyticsRepository) {
		t.Helper()
		costs, err := reader.getCostByAccount(30)
		if err != nil || costs["account"] != 4 {
			t.Fatalf("cost after cutover = %+v, err = %v", costs, err)
		}
	}
	assertCost(duck)
	if err := duck.migrateSQLiteCosts(t.Context(), filepath.Join(dir, "missing.db"), today); err != nil {
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
	if err != nil || len(costs) != 0 {
		t.Fatalf("deleted account cost remains in restored analytics: %+v, %v", costs, err)
	}
}
