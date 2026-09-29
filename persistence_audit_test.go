package main

// Restart/persistence adversarial tests: corrupted, truncated, and locked
// database files must fail gracefully (error, not panic or hard crash),
// because DuckDB and Bolt sit behind CGO/native code paths where a crash
// takes the whole proxy down.

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

// TestAuditDuckAnalyticsCorruptDatabaseFailsGracefully writes garbage bytes
// into a usage.duckdb file and verifies newDuckAnalytics reports an error
// instead of crashing the process through the native DuckDB library.
func TestAuditDuckAnalyticsCorruptDatabaseFailsGracefully(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.duckdb")
	garbage := strings.Repeat("not-a-duckdb-database! ", 500)
	if err := os.WriteFile(path, []byte(garbage), 0o600); err != nil {
		t.Fatal(err)
	}
	store := testUsageStore(t)
	defer store.Close()

	duck, err := newDuckAnalytics(path, store.db)
	if err == nil {
		duck.Close()
		t.Fatal("opening a corrupt DuckDB file unexpectedly succeeded")
	}
}

// TestAuditDuckAnalyticsTruncatedDatabaseFailsGracefully truncates a valid
// database mid-file (simulating a crash between write and flush) and
// verifies reopen reports an error rather than crashing.
func TestAuditDuckAnalyticsTruncatedDatabaseFailsGracefully(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "usage.duckdb")
	store := testUsageStore(t)
	defer store.Close()
	duck, err := newDuckAnalytics(path, store.db)
	if err != nil {
		t.Fatal(err)
	}
	if err := duck.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Size() < 4096 {
		t.Skipf("database too small to truncate meaningfully: %d bytes", info.Size())
	}
	if err := os.Truncate(path, info.Size()/3); err != nil {
		t.Fatal(err)
	}

	reopened, err := newDuckAnalytics(path, store.db)
	if err == nil {
		reopened.Close()
		t.Fatal("opening a truncated DuckDB file unexpectedly succeeded")
	}
}

// TestAuditUsageStoreTruncatedBoltFailsGracefully
// BUG-AUDIT-109 (cross-platform): a proxy.db truncated by a crash between
// write and flush makes bbolt panic ("invalid freelist page") inside
// newUsageStore, which main.go calls at startup — the process dies with a
// raw Go panic instead of a clean, actionable error, and keeps panicking on
// every restart until an operator repairs the file by hand.
// Expected: newUsageStore returns an error for a corrupt store.
// Actual: panic propagates out of bbolt.Open's freelist load.
func TestAuditUsageStoreTruncatedBoltFailsGracefully(t *testing.T) {
	// Not t.TempDir(): the panicking bbolt leaves the mmap open, which would
	// turn the framework's RemoveAll cleanup into a second failure.
	dir, err := os.MkdirTemp("", "codex-pool-bolt-truncate-*")
	if err != nil {
		t.Fatal(err)
	}
	defer os.RemoveAll(dir)
	path := filepath.Join(dir, "proxy.db")
	store, err := newUsageStore(path, 30)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Truncate(path, info.Size()/2); err != nil {
		t.Fatal(err)
	}

	panicked := true
	func() {
		defer func() {
			if r := recover(); r != nil {
				t.Fatalf("BUG-AUDIT-109: bbolt panicked on truncated store: %v", r)
			}
			panicked = false
		}()
		if reopened, err := newUsageStore(path, 30); err == nil {
			defer reopened.Close()
			t.Fatal("opening a truncated Bolt store unexpectedly succeeded")
		}
	}()
	_ = panicked
}

// TestAuditUsageStoreReadOnlyFileFailsGracefully marks the Bolt file
// read-only (Windows READONLY attribute) and verifies a clean error. On
// Windows, os.Chmod(0444) sets the attribute; opening for write must fail
// with a permission error rather than hanging or panicking.
func TestAuditUsageStoreReadOnlyFileFailsGracefully(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proxy.db")
	store, err := newUsageStore(path, 30)
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(path, 0o444); err != nil {
		t.Fatal(err)
	}
	defer os.Chmod(path, 0o600)

	reopened, err := newUsageStore(path, 30)
	if err == nil {
		reopened.Close()
		t.Fatal("opening a read-only Bolt store for write unexpectedly succeeded")
	}
	if reopened != nil {
		reopened.Close()
	}
}
