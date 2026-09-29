package main

// Adversarial DuckDB/CGO tests for Windows path handling: spaces, non-ASCII
// characters, and paths exceeding the classic MAX_PATH limit.

import (
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// TestAuditDuckDBSpacedUnicodePathRoundTrip opens the analytics store under
// a directory containing spaces and non-ASCII characters (the documented
// Windows deployment shape), records a usage fact, reopens the store, and
// verifies the fact reconciles.
func TestAuditDuckDBSpacedUnicodePathRoundTrip(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "codex pool analytics ã é ü", "data")
	store := testUsageStore(t)
	defer store.Close()

	duck, err := newDuckAnalytics(filepath.Join(dir, "usage.duckdb"), store.db)
	if err != nil {
		t.Fatalf("open DuckDB in spaced unicode path: %v", err)
	}
	now := time.Now().UTC()
	usage := RequestUsage{Timestamp: now, AccountID: "a", AccountType: AccountTypeCodex, UserID: "p1", ClientCredentialID: "mac", ProxyRequestID: "req-space", InputTokens: 7, OutputTokens: 3, BillableTokens: 10}
	if err := store.recordWithCost(usage, 0.5); err != nil {
		t.Fatal(err)
	}
	if err := duck.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	reopened, err := newDuckAnalytics(filepath.Join(dir, "usage.duckdb"), store.db)
	if err != nil {
		t.Fatalf("reopen DuckDB in spaced unicode path: %v", err)
	}
	defer reopened.Close()
	result, err := reopened.Reconcile(now.Add(-time.Minute), now.Add(time.Minute))
	if err != nil {
		t.Fatalf("reconcile: %v", err)
	}
	if !result.Clean {
		t.Fatalf("reconciliation after reopen not clean: %+v", result)
	}
}

// TestAuditDuckDBLongPath opens the DuckDB store at a path beyond the
// classic 260-character MAX_PATH limit. Go's os package handles long paths
// via \\?\ prefixes, but the DuckDB C++ filesystem layer may not; this test
// documents which side of that boundary the deployment sits on.
func TestAuditDuckDBLongPath(t *testing.T) {
	base := t.TempDir()
	if len(base) > 200 {
		t.Skipf("temp base already too long for a meaningful long-path test: %d", len(base))
	}
	deep := base
	// Grow a chain of directories until the final file path exceeds 260 chars.
	segment := strings.Repeat("d", 40)
	path := filepath.Join(deep, "usage.duckdb")
	for len(path) <= 260 {
		deep = filepath.Join(deep, segment)
		path = filepath.Join(deep, "usage.duckdb")
	}

	store := testUsageStore(t)
	defer store.Close()
	duck, err := newDuckAnalytics(path, store.db)
	if err != nil {
		t.Fatalf("DuckDB cannot open a >260 char path (len=%d): %v", len(path), err)
	}
	if err := duck.Close(); err != nil {
		t.Fatalf("close long-path DuckDB: %v", err)
	}
}
