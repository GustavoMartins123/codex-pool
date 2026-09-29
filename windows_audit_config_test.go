package main

// Adversarial audit tests for the config watcher and the paired backup
// restore path. Tests that intentionally fail against current production
// code carry a BUG-AUDIT-XXX marker and are listed in
// docs/windows-audit-report.md.

import (
	"database/sql"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"
	"go.etcd.io/bbolt"
)

func newAuditConfigWatcher(t *testing.T, poolDir, configPath string) (*poolWatcher, *proxyHandler) {
	t.Helper()
	h := &proxyHandler{cfg: &config{}, pool: newPoolState(nil, false)}
	pw, err := newPoolWatcher(poolDir, configPath, h)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(pw.close)
	return pw, h
}

// TestAuditWatcherSurvivesRepeatedAtomicConfigReplace
// BUG-AUDIT-106 regression (cross-platform): atomic saves replace
// config.toml via rename, which supersedes any file-level watch after the
// first replace. The watcher now observes the config's parent directory, so
// every replace — and rename, delete, delete+recreate cycles — must keep
// hot-reloading. Editors and deployment tools commonly save atomically.
func TestAuditWatcherSurvivesRepeatedAtomicConfigReplace(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("debug = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, h := newAuditConfigWatcher(t, "", cfgPath)

	for i, want := range []bool{true, false, true} {
		content := "debug = false\n"
		if want {
			content = "debug = true\n"
		}
		if err := writeFileAtomic(cfgPath, []byte(content)); err != nil {
			t.Fatalf("replace %d: %v", i, err)
		}
		if !auditWaitFor(t, 3*time.Second, func() bool { return h.cfg.debug.Load() == want }) {
			t.Fatalf("BUG-AUDIT-106: config replace %d (debug=%v) not hot-reloaded within 3s (file watch superseded by earlier atomic replace)", i, want)
		}
	}

	// Rename the config away (with new content) and back — the watch is on
	// the directory, so the restored file must be observed and reloaded.
	if err := os.Rename(cfgPath, cfgPath+".away"); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath+".away", []byte("debug = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(cfgPath+".away", cfgPath); err != nil {
		t.Fatal(err)
	}
	if !auditWaitFor(t, 3*time.Second, func() bool { return !h.cfg.debug.Load() }) {
		t.Fatal("BUG-AUDIT-106: config events lost after rename away/back cycle")
	}

	// Delete and recreate: directory watch must still deliver events.
	if err := os.Remove(cfgPath); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(cfgPath, []byte("debug = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !auditWaitFor(t, 3*time.Second, func() bool { return h.cfg.debug.Load() }) {
		t.Fatal("BUG-AUDIT-106: config events lost after delete/recreate cycle")
	}
}

// TestAuditWatcherReloadsCRLFConfigFile writes config.toml with Windows
// CRLF line endings and asserts the hot-reload still parses it. No parser or
// watcher path may depend on Unix LF-only files.
func TestAuditWatcherReloadsCRLFConfigFile(t *testing.T) {
	dir := t.TempDir()
	cfgPath := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(cfgPath, []byte("debug = false\r\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	_, h := newAuditConfigWatcher(t, "", cfgPath)

	if err := writeFileAtomic(cfgPath, []byte("debug = true\r\n")); err != nil {
		t.Fatal(err)
	}
	if !auditWaitFor(t, 3*time.Second, func() bool { return h.cfg.debug.Load() }) {
		t.Fatal("CRLF config.toml hot-reload did not apply debug=true")
	}
}

// buildPairedRestoreFixture creates a matched Bolt+DuckDB pair ("before"),
// a paired backup, then advances both stores to "after". Restores from the
// manifest should bring both back to "before"; failed restores must leave
// both at "after".
func buildPairedRestoreFixture(t *testing.T) (manifest, boltPath, duckPath string) {
	t.Helper()
	directory := t.TempDir()
	boltPath = filepath.Join(directory, "proxy.db")
	duckPath = filepath.Join(directory, "usage.duckdb")
	backupDir := filepath.Join(directory, "backups")

	bolt, err := bbolt.Open(boltPath, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := bolt.Update(func(tx *bbolt.Tx) error {
		bucket, err := tx.CreateBucketIfNotExists([]byte("proof"))
		if err != nil {
			return err
		}
		return bucket.Put([]byte("value"), []byte("before"))
	}); err != nil {
		t.Fatal(err)
	}
	if err := bolt.Close(); err != nil {
		t.Fatal(err)
	}
	duck, err := sql.Open("duckdb", duckPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := duck.Exec("CREATE TABLE proof(value VARCHAR); INSERT INTO proof VALUES ('before')"); err != nil {
		t.Fatal(err)
	}
	if err := duck.Close(); err != nil {
		t.Fatal(err)
	}

	manifest, err = createPairedBackup(boltPath, duckPath, backupDir)
	if err != nil {
		t.Fatal(err)
	}

	bolt, err = bbolt.Open(boltPath, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := bolt.Update(func(tx *bbolt.Tx) error { return tx.Bucket([]byte("proof")).Put([]byte("value"), []byte("after")) }); err != nil {
		t.Fatal(err)
	}
	if err := bolt.Close(); err != nil {
		t.Fatal(err)
	}
	duck, err = sql.Open("duckdb", duckPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := duck.Exec("UPDATE proof SET value='after'"); err != nil {
		t.Fatal(err)
	}
	if err := duck.Close(); err != nil {
		t.Fatal(err)
	}
	return manifest, boltPath, duckPath
}

func readBoltProof(t *testing.T, path string) string {
	t.Helper()
	bolt, err := bbolt.Open(path, 0o600, &bbolt.Options{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer bolt.Close()
	value := ""
	if err := bolt.View(func(tx *bbolt.Tx) error {
		value = string(tx.Bucket([]byte("proof")).Get([]byte("value")))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	return value
}

func readDuckProof(t *testing.T, path string) string {
	t.Helper()
	duck, err := sql.Open("duckdb", path)
	if err != nil {
		t.Fatal(err)
	}
	defer duck.Close()
	var value string
	if err := duck.QueryRow("SELECT value FROM proof").Scan(&value); err != nil {
		t.Fatal(err)
	}
	return value
}

// assertNoRestoreLeftovers fails when .restore/.prerestore staging files
// survive a restore attempt.
func assertNoRestoreLeftovers(t *testing.T, boltPath, duckPath string) {
	t.Helper()
	for _, leftover := range []string{boltPath + ".restore", duckPath + ".restore", boltPath + ".prerestore", duckPath + ".prerestore"} {
		if _, err := os.Stat(leftover); !os.IsNotExist(err) {
			t.Fatalf("staging file %s survived the restore attempt", leftover)
		}
	}
}

// failRenameTo wraps os.Rename and fails every call whose destination
// matches target — used to inject a failure at an exact swap point.
func failRenameTo(target string) func(string, string) error {
	return func(oldPath, newPath string) error {
		if newPath == target {
			return fmt.Errorf("injected rename failure for %s", newPath)
		}
		return os.Rename(oldPath, newPath)
	}
}

// TestAuditRestorePairedBackupRollsBackWhenSecondSwapFails
// BUG-AUDIT-105 regression (cross-platform, Windows-amplified): when the
// DuckDB swap fails after the Bolt swap succeeded — on Windows any open
// handle without FILE_SHARE_DELETE (indexer, antivirus, a stray duckdb
// client) blocks the rename — the restore must roll Bolt back so the stores
// stay a matched pair, instead of leaving Bolt on the backup snapshot while
// DuckDB keeps current data. The failure is injected at the rename step
// because no cross-platform filesystem state fails exactly the second swap
// after the first one succeeded; the Windows-realistic variant runs in
// windows_restore_audit_test.go.
func TestAuditRestorePairedBackupRollsBackWhenSecondSwapFails(t *testing.T) {
	manifest, boltPath, duckPath := buildPairedRestoreFixture(t)
	previous := restoreRename
	restoreRename = failRenameTo(duckPath)
	t.Cleanup(func() { restoreRename = previous })

	err := restorePairedBackup(manifest, boltPath, duckPath)
	if err == nil {
		t.Fatal("restore unexpectedly succeeded with an injected DuckDB swap failure")
	}
	if !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("restore error does not report rollback: %v", err)
	}
	if value := readBoltProof(t, boltPath); value != "after" {
		t.Fatalf("BUG-AUDIT-105: torn restore — Bolt left at %q although the DuckDB swap failed", value)
	}
	if value := readDuckProof(t, duckPath); value != "after" {
		t.Fatalf("BUG-AUDIT-105: DuckDB left at %q after a failed restore", value)
	}
	assertNoRestoreLeftovers(t, boltPath, duckPath)
}

// TestAuditRestorePairedBackupLeavesStoresUntouchedWhenFirstSwapFails covers
// the first-swap failure: nothing has been replaced yet, so both stores must
// remain exactly as they were.
func TestAuditRestorePairedBackupLeavesStoresUntouchedWhenFirstSwapFails(t *testing.T) {
	manifest, boltPath, duckPath := buildPairedRestoreFixture(t)
	previous := restoreRename
	restoreRename = failRenameTo(boltPath)
	t.Cleanup(func() { restoreRename = previous })

	if err := restorePairedBackup(manifest, boltPath, duckPath); err == nil {
		t.Fatal("restore unexpectedly succeeded with an injected Bolt swap failure")
	}
	if value := readBoltProof(t, boltPath); value != "after" {
		t.Fatalf("BUG-AUDIT-105: Bolt replaced (%q) although its swap failed", value)
	}
	if value := readDuckProof(t, duckPath); value != "after" {
		t.Fatalf("BUG-AUDIT-105: DuckDB replaced (%q) although the Bolt swap failed", value)
	}
	assertNoRestoreLeftovers(t, boltPath, duckPath)
}

// TestAuditRestorePairedBackupFailsCleanlyWhenDuckPathBlocked exercises a
// real filesystem obstacle: a directory sitting at the DuckDB path cannot
// be snapshotted or replaced by a file on any platform. The restore must
// fail cleanly BEFORE touching Bolt, with no staging files left behind.
func TestAuditRestorePairedBackupFailsCleanlyWhenDuckPathBlocked(t *testing.T) {
	manifest, boltPath, duckPath := buildPairedRestoreFixture(t)

	blocker := filepath.Join(filepath.Dir(duckPath), "usage-blocked")
	if err := os.Mkdir(blocker, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(duckPath, filepath.Join(blocker, "usage.duckdb")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(duckPath, 0o700); err != nil {
		t.Fatal(err)
	}

	if err := restorePairedBackup(manifest, boltPath, duckPath); err == nil {
		t.Fatal("restore unexpectedly succeeded with a directory at the DuckDB path")
	}
	if value := readBoltProof(t, boltPath); value != "after" {
		t.Fatalf("BUG-AUDIT-105: torn restore — Bolt was replaced (%q) although the DuckDB path was blocked", value)
	}
	if value := readDuckProof(t, filepath.Join(blocker, "usage.duckdb")); value != "after" {
		t.Fatalf("original DuckDB data damaged by the failed restore: %q", value)
	}
	assertNoRestoreLeftovers(t, boltPath, duckPath)
}
