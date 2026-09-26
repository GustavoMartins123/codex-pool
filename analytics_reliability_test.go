package main

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

func TestAccountingGapPersistsAfterRecovery(t *testing.T) {
	store := testUsageStore(t)
	started := time.Now().UTC().Add(-time.Minute)
	store.openAccountingGap(started, errors.New("disk full"))
	gaps, active, err := store.accountingGaps()
	if err != nil {
		t.Fatal(err)
	}
	if len(gaps) != 0 || active == nil || !active.StartedAt.Equal(started) {
		t.Fatalf("gaps=%+v active=%+v", gaps, active)
	}
	store.closeAccountingGap(time.Now().UTC())
	gaps, active, err = store.accountingGaps()
	if err != nil {
		t.Fatal(err)
	}
	if active != nil || len(gaps) != 1 || gaps[0].EndedAt == nil {
		t.Fatalf("gaps=%+v active=%+v", gaps, active)
	}
}

func TestActiveAccountingGapSurvivesRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "proxy.db")
	store, err := newUsageStore(path, 30)
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now().UTC().Add(-time.Minute)
	store.openAccountingGap(started, errors.New("bolt unavailable"))
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	reopened, err := newUsageStore(path, 30)
	if err != nil {
		t.Fatal(err)
	}
	defer reopened.Close()
	_, active, err := reopened.accountingGaps()
	if err != nil {
		t.Fatal(err)
	}
	if active == nil || !active.StartedAt.Equal(started) {
		t.Fatalf("active gap after restart = %+v", active)
	}
}

func TestAnalyticsReconciliationIncludesDurableOutbox(t *testing.T) {
	store := testUsageStore(t)
	duck, err := newDuckAnalytics(filepath.Join(t.TempDir(), "usage.duckdb"), store.db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = duck.Close() })
	now := time.Now().UTC()
	usage := RequestUsage{Timestamp: now, AccountID: "a", AccountType: AccountTypeCodex, UserID: "p1", ClientCredentialID: "mac", ProxyRequestID: "req-reconcile", InputTokens: 11, CachedInputTokens: 3, OutputTokens: 5, BillableTokens: 19}
	if err := store.recordWithCost(usage, 0.42); err != nil {
		t.Fatal(err)
	}
	result, err := duck.Reconcile(now.Add(-time.Minute), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if !result.Clean || result.Bolt != result.Ledger {
		t.Fatalf("reconciliation=%+v", result)
	}
	if err := store.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket([]byte(bucketAnalyticsOutbox))
		key, _ := bucket.Cursor().First()
		return bucket.Delete(key)
	}); err != nil {
		t.Fatal(err)
	}
	result, err = duck.Reconcile(now.Add(-time.Minute), now.Add(time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if result.Clean {
		t.Fatal("reconciliation failed to detect a missing fact")
	}
}

func TestDuckAnalyticsReplayIsIdempotent(t *testing.T) {
	store := testUsageStore(t)
	duck, err := newDuckAnalytics(filepath.Join(t.TempDir(), "usage.duckdb"), store.db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = duck.Close() })
	now := time.Now().UTC()
	fact := AnalyticsFact{EventID: "event-fixed", ProxyRequestID: "request-fixed", ObservedAt: now, PrincipalID: "p1", ClientCredentialID: "mac", AccountID: "a", AccountType: "codex", NormalizationVersion: "v1", BillableTokens: 10, PricingVersion: "v1", UsageCompleteness: "complete", Source: "live", SourceGrain: "request"}
	if err := store.db.Update(func(tx *bbolt.Tx) error { return putAnalyticsOutbox(tx, fact) }); err != nil {
		t.Fatal(err)
	}
	if err := duck.drain(10); err != nil {
		t.Fatal(err)
	}
	if err := store.db.Update(func(tx *bbolt.Tx) error { return putAnalyticsOutbox(tx, fact) }); err != nil {
		t.Fatal(err)
	}
	if err := duck.drain(10); err != nil {
		t.Fatal(err)
	}
	rows, err := duck.UserHourly(context.Background(), "p1", now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].BillableTokens != 10 || rows[0].RequestCount != 1 {
		t.Fatalf("rows=%+v", rows)
	}
}

// Regression test: the outbox drainer's DuckDB writes and the HTTP-facing
// readers share one connection pool, so a drain must exclude concurrent
// readers instead of interleaving on it. Readers race the drain; the final
// ledger must still contain every fact exactly once.
func TestDuckAnalyticsDrainExcludesConcurrentReaders(t *testing.T) {
	store := testUsageStore(t)
	duck, err := newDuckAnalytics(filepath.Join(t.TempDir(), "usage.duckdb"), store.db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = duck.Close() })

	now := time.Now().UTC()
	const facts = 60
	for i := 0; i < facts; i++ {
		fact := AnalyticsFact{
			EventID: fmt.Sprintf("event-race-%d", i), ProxyRequestID: fmt.Sprintf("request-race-%d", i),
			ObservedAt: now, PrincipalID: "p1", ClientCredentialID: "mac", AccountID: "a", AccountType: "codex",
			NormalizationVersion: "v1", BillableTokens: 1, PricingVersion: "v1", UsageCompleteness: "complete",
			Source: "live", SourceGrain: "request",
		}
		if err := store.db.Update(func(tx *bbolt.Tx) error { return putAnalyticsOutbox(tx, fact) }); err != nil {
			t.Fatal(err)
		}
	}

	var wg sync.WaitGroup
	wg.Add(1)
	go func() {
		defer wg.Done()
		for i := 0; i < facts/16+1; i++ {
			if err := duck.drain(16); err != nil {
				t.Errorf("drain: %v", err)
				return
			}
		}
	}()
	for r := 0; r < 3; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := 0; i < 5; i++ {
				rows, err := duck.UserHourly(context.Background(), "p1", now.Add(-time.Minute))
				if err != nil {
					t.Errorf("user hourly: %v", err)
					return
				}
				for _, row := range rows {
					if row.BillableTokens < 0 || row.RequestCount > facts {
						t.Errorf("reader observed impossible ledger state: %+v", row)
						return
					}
				}
			}
		}()
	}
	wg.Wait()

	rows, err := duck.UserHourly(context.Background(), "p1", now.Add(-time.Minute))
	if err != nil {
		t.Fatal(err)
	}
	if len(rows) != 1 || rows[0].BillableTokens != facts || rows[0].RequestCount != facts {
		t.Fatalf("final ledger rows=%+v, want exactly %d tokens and requests", rows, facts)
	}
}
