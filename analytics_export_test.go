package main

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
	"time"
)

func exportTestAnalytics(t *testing.T, usages ...RequestUsage) *DuckAnalytics {
	t.Helper()
	store := testUsageStore(t)
	duck, err := newDuckAnalytics(filepath.Join(t.TempDir(), "usage.duckdb"), store.db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = duck.Close() })
	for i, usage := range usages {
		if err := store.recordWithCost(usage, float64(i+1)/100); err != nil {
			t.Fatal(err)
		}
	}
	duck.Notify()
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		count, _, err := duck.ExportUsage(context.Background(), UsageExportQuery{Since: time.Unix(0, 0), Limit: len(usages) + 1}, func(UsageExportRow) error { return nil })
		if err == nil && count == len(usages) {
			return duck
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("facts not drained")
	return nil
}

func collectExport(t *testing.T, duck *DuckAnalytics, query UsageExportQuery) ([]UsageExportRow, bool) {
	t.Helper()
	rows := make([]UsageExportRow, 0)
	count, truncated, err := duck.ExportUsage(context.Background(), query, func(row UsageExportRow) error {
		rows = append(rows, row)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if count != len(rows) {
		t.Fatalf("count = %d, emitted %d", count, len(rows))
	}
	return rows, truncated
}

func TestExportUsageScopesPrincipalAndWindow(t *testing.T) {
	now := time.Now().UTC().Truncate(time.Second)
	duck := exportTestAnalytics(t,
		RequestUsage{Timestamp: now.Add(-time.Hour), AccountID: "acc-1", AccountType: AccountTypeCodex, UserID: "p1", ClientCredentialID: "cred-1", OriginID: "origin-hash", ProxyRequestID: "req-1", PlanType: "plus", Model: "gpt-test", InputTokens: 10, CachedInputTokens: 2, CacheCreationTokens: 3, OutputTokens: 5, ReasoningTokens: 1, BillableTokens: 15},
		RequestUsage{Timestamp: now.Add(-48 * time.Hour), AccountID: "acc-1", AccountType: AccountTypeCodex, UserID: "p1", ClientCredentialID: "cred-1", ProxyRequestID: "req-2", Model: "gpt-test", BillableTokens: 99},
		RequestUsage{Timestamp: now.Add(-30 * time.Minute), AccountID: "acc-2", AccountType: AccountTypeClaude, UserID: "p2", ClientCredentialID: "cred-2", ProxyRequestID: "req-3", Model: "claude-test", BillableTokens: 7},
	)

	all, truncated := collectExport(t, duck, UsageExportQuery{Since: now.Add(-2 * time.Hour), Limit: 100})
	if truncated || len(all) != 2 {
		t.Fatalf("rows = %d, truncated = %v", len(all), truncated)
	}
	if all[0].PrincipalID != "p2" || all[1].PrincipalID != "p1" {
		t.Fatalf("expected newest first, got %+v", all)
	}
	row := all[1]
	if row.ClientCredentialID != "cred-1" || row.OriginID != "origin-hash" || row.AccountID != "acc-1" ||
		row.AccountType != string(AccountTypeCodex) || row.PlanType != "plus" || row.Model != "gpt-test" {
		t.Fatalf("row identity = %+v", row)
	}
	if row.InputTokens != 10 || row.CacheReadTokens != 2 || row.CacheCreationTokens != 3 ||
		row.OutputTokens != 5 || row.ReasoningTokens != 1 || row.BillableTokens != 15 {
		t.Fatalf("row tokens = %+v", row)
	}
	if row.APIEquivalentCostUSD != 0.01 {
		t.Fatalf("cost = %v", row.APIEquivalentCostUSD)
	}
	if !row.ObservedAt.Equal(now.Add(-time.Hour)) {
		t.Fatalf("observed_at = %v", row.ObservedAt)
	}

	scoped, _ := collectExport(t, duck, UsageExportQuery{PrincipalID: "p1", Since: now.Add(-2 * time.Hour), Limit: 100})
	if len(scoped) != 1 || scoped[0].PrincipalID != "p1" {
		t.Fatalf("principal scope leaked: %+v", scoped)
	}
}

func TestExportUsageCapsRowsAndFlagsTruncation(t *testing.T) {
	now := time.Now().UTC()
	duck := exportTestAnalytics(t,
		RequestUsage{Timestamp: now.Add(-3 * time.Minute), AccountID: "a", AccountType: AccountTypeCodex, UserID: "p1", ProxyRequestID: "req-1", BillableTokens: 1},
		RequestUsage{Timestamp: now.Add(-2 * time.Minute), AccountID: "a", AccountType: AccountTypeCodex, UserID: "p1", ProxyRequestID: "req-2", BillableTokens: 2},
		RequestUsage{Timestamp: now.Add(-time.Minute), AccountID: "a", AccountType: AccountTypeCodex, UserID: "p1", ProxyRequestID: "req-3", BillableTokens: 3},
	)

	capped, truncated := collectExport(t, duck, UsageExportQuery{Since: now.Add(-time.Hour), Limit: 2})
	if !truncated || len(capped) != 2 {
		t.Fatalf("rows = %d, truncated = %v", len(capped), truncated)
	}
	if capped[0].BillableTokens != 3 || capped[1].BillableTokens != 2 {
		t.Fatalf("cap dropped the newest rows: %+v", capped)
	}

	full, truncated := collectExport(t, duck, UsageExportQuery{Since: now.Add(-time.Hour), Limit: 3})
	if truncated || len(full) != 3 {
		t.Fatalf("rows = %d, truncated = %v", len(full), truncated)
	}
}

func TestExportUsagePropagatesEmitError(t *testing.T) {
	now := time.Now().UTC()
	duck := exportTestAnalytics(t, RequestUsage{Timestamp: now.Add(-time.Minute), AccountID: "a", AccountType: AccountTypeCodex, UserID: "p1", ProxyRequestID: "req-1", BillableTokens: 1})
	sentinel := errors.New("client went away")
	count, _, err := duck.ExportUsage(context.Background(), UsageExportQuery{Since: now.Add(-time.Hour), Limit: 10}, func(UsageExportRow) error { return sentinel })
	if !errors.Is(err, sentinel) {
		t.Fatalf("err = %v", err)
	}
	if count != 0 {
		t.Fatalf("count = %d", count)
	}
}