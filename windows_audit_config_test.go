package main

// Adversarial audit tests for the config watcher and the paired backup
// restore path. Tests that intentionally fail against current production
// code carry a BUG-AUDIT-XXX marker and are listed in
// docs/windows-audit-report.md.

import (
	"database/sql"
	"os"
	"path/filepath"
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
// BUG-AUDIT-106 (cross-platform): fsnotify watches config.toml itself, so an
// atomic replace (temp+rename) supersedes the watched inode/handle. The
// first replace still delivers a terminal event for the old file and reloads
// once; every later save is silently ignored until restart. Editors and
// deployment tools commonly save atomically.
// Expected: every atomic replace of config.toml hot-reloads.
// Actual: only the first replace is observed.
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

// TestAuditWatcherMatchesConfigPathCaseInsensitively
// BUG-AUDIT-104 (Windows-only): NTFS is case-insensitive, so newPoolWatcher
// happily watches a CONFIG_PATH whose casing differs from the on-disk name,
// but ReadDirectoryChangesW reports events with the on-disk casing and
// handleEvent compares with `==`. The config change is then misclassified as
// a pool event and the hot-reload is silently lost. On Linux a wrong-case
// path fails Add outright and is at least logged as unwatchable.
// Expected: a modify event for the config file (any casing) reloads config.
// Actual: debug never flips.
func TestAuditWatcherMatchesConfigPathCaseInsensitively(t *testing.T) {
	dir := t.TempDir()
	onDisk := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(onDisk, []byte("debug = false\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	requested := filepath.Join(dir, "Config.TOML")
	_, h := newAuditConfigWatcher(t, "", requested)

	if err := os.WriteFile(onDisk, []byte("debug = true\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if !auditWaitFor(t, 3*time.Second, func() bool { return h.cfg.debug.Load() }) {
		t.Fatal("BUG-AUDIT-104: config hot-reload lost because event name casing differs from CONFIG_PATH")
	}
}

// TestAuditRestorePairedBackupRollsBackWhenSecondRenameFails
// BUG-AUDIT-105 (cross-platform, Windows-amplified): restorePairedBackup
// renames bolt first and duck second with no rollback. When the duck rename
// fails — on Windows any open handle without FILE_SHARE_DELETE (indexer,
// antivirus, a stray duckdb client) blocks it, while POSIX only fails on
// exotic conditions — the paired backup invariant is torn: Bolt holds the
// restored snapshot while DuckDB keeps current data, and the operator is
// left in a state neither backup nor current.
// Expected: a failed restore leaves both stores untouched.
// Actual: Bolt is already replaced when the DuckDB rename fails.
func TestAuditRestorePairedBackupRollsBackWhenSecondRenameFails(t *testing.T) {
	directory := t.TempDir()
	boltPath := filepath.Join(directory, "proxy.db")
	duckPath := filepath.Join(directory, "usage.duckdb")
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

	manifest, err := createPairedBackup(boltPath, duckPath, backupDir)
	if err != nil {
		t.Fatal(err)
	}

	// Advance both stores past the backup.
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

	// Force only the DuckDB rename to fail: a directory at the target path
	// cannot be replaced by a file rename on any platform.
	blocker := filepath.Join(directory, "usage-blocked")
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

	// The paired invariant: a failed restore must leave Bolt untouched.
	bolt, err = bbolt.Open(boltPath, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	value := ""
	if err := bolt.View(func(tx *bbolt.Tx) error {
		value = string(tx.Bucket([]byte("proof")).Get([]byte("value")))
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	_ = bolt.Close()
	if value != "after" {
		t.Fatalf("BUG-AUDIT-105: torn restore — Bolt was replaced (%q) although the DuckDB rename failed", value)
	}
}
