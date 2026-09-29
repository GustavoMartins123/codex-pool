package main

// Startup and persistence regressions for bare-executable deployments
// (Windows runs the .exe directly, no Docker image that pre-creates dirs).

import (
	"os"
	"path/filepath"
	"testing"
)

// TestAuditFreshDirectoryStartupCreatesStorage
// BUG-AUDIT-108: a bare codex-pool executable started in a fresh working
// directory — the documented Windows deployment ("Run the binary from a
// directory where you want pool/, data/ ... to live") — crashes because
// newUsageStore opens ./data/proxy.db without creating the parent
// directory. The Docker image masks this with `mkdir -p /app/data`, so the
// bug only surfaces outside Docker, i.e. Windows.
// Expected: opening the usage store in a fresh directory succeeds (or
// creates the directory).
// Actual: bbolt.Open fails with "The system cannot find the path
// specified." and startup aborts.
func TestAuditFreshDirectoryStartupCreatesStorage(t *testing.T) {
	dir := t.TempDir()
	working := filepath.Join(dir, "fresh-run")
	if err := mkdirForAudit(working); err != nil {
		t.Fatal(err)
	}
	storePath := filepath.Join(working, "data", "proxy.db")

	store, err := newUsageStore(storePath, 30)
	if err != nil {
		t.Fatalf("BUG-AUDIT-108: fresh-directory startup cannot open usage store: %v", err)
	}
	defer store.Close()
}

// mkdirForAudit mirrors the operator's step of creating the working
// directory only; data/ must not need to exist beforehand.
func mkdirForAudit(path string) error {
	return os.MkdirAll(path, 0o755)
}
