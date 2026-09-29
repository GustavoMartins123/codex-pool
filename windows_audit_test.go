package main

// Adversarial Windows audit tests. These target hidden POSIX assumptions in
// the filesystem, atomic-write, and watcher layers. Tests that intentionally
// fail against current production code carry a BUG-AUDIT-XXX marker in their
// failure message and are listed in docs/windows-audit-report.md.

import (
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// auditWaitFor polls cond until it holds or the timeout elapses.
func auditWaitFor(t *testing.T, timeout time.Duration, cond func() bool) bool {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if cond() {
			return true
		}
		time.Sleep(20 * time.Millisecond)
	}
	return cond()
}

// TestAuditCredentialRoundTripInSpacedUnicodeDirectory verifies the credential
// read/write/vault path survives a Windows directory containing spaces and
// non-ASCII characters. This is the documented deployment shape on Windows.
func TestAuditCredentialRoundTripInSpacedUnicodeDirectory(t *testing.T) {
	base := t.TempDir()
	dir := filepath.Join(base, "codex pool audit ã é ü", "pool")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "conta um.json")
	secret := `{"tokens":{"access_token":"SECRET_SHOULD_NEVER_APPEAR_12345","refresh_token":"r"}}`
	if err := writeAccountFile(file, []byte(secret)); err != nil {
		t.Fatalf("writeAccountFile in spaced unicode dir: %v", err)
	}
	got, err := readAccountFile(file)
	if err != nil {
		t.Fatalf("readAccountFile in spaced unicode dir: %v", err)
	}
	if string(got) != secret {
		t.Fatalf("credential round-trip corrupted payload: %q", got)
	}
}

// TestAuditCredentialFilesAreNotObservablyTruncatedByWatcherReload hammers the
// atomic writer while concurrent Go readers loop, mirroring a token-refresh
// save racing the watcher's hot-reload read of the same file. On POSIX the
// renames always succeed and readers never see partial JSON.
//
// BUG-AUDIT-001 (Windows-only, same root cause as the locked-file tests):
// Go's os.Open/os.ReadFile hold the destination without FILE_SHARE_DELETE,
// so every os.Rename in writeFileAtomic fails with "Access is denied" while
// a reload is reading the file, dropping the refreshed credential.
// Expected: all writes succeed and readers only ever see complete JSON.
// Actual: writes fail as soon as a reader has the file open.
func TestAuditCredentialFilesAreNotObservablyTruncatedByWatcherReload(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "one.json")
	payload := []byte(`{"api_key":"` + strings.Repeat("k", 4096) + `"}`)
	if err := writeAccountFile(file, payload); err != nil {
		t.Fatal(err)
	}
	stop := make(chan struct{})
	var wg sync.WaitGroup
	var corrupt sync.Map
	for r := 0; r < 4; r++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				select {
				case <-stop:
					return
				default:
				}
				data, err := os.ReadFile(file)
				if err != nil {
					corrupt.Store(err.Error(), true)
					return
				}
				if len(data) == 0 {
					corrupt.Store("empty read", true)
					return
				}
				var probe map[string]any
				if json.Unmarshal(data, &probe) != nil {
					corrupt.Store("partial json", true)
					return
				}
			}
		}()
	}
	for i := 0; i < 150; i++ {
		if err := writeAccountFile(file, payload); err != nil {
			t.Fatalf("BUG-AUDIT-001: iteration %d: atomic write failed while reload readers held the file: %v", i, err)
		}
	}
	close(stop)
	wg.Wait()
	corrupt.Range(func(k, _ any) bool {
		t.Errorf("reader observed %v", k)
		return false
	})
}

// TestAuditWatcherHotReloadsAtomicCredentialSave exercises the real fsnotify
// path on the current platform: a credential replaced via the atomic
// temp+rename writer must trigger a pool reload after the debounce window.
// Windows emits a different event sequence than Linux for renames, so this is
// a platform regression guard.
func TestAuditWatcherHotReloadsAtomicCredentialSave(t *testing.T) {
	dir := t.TempDir()
	if err := os.MkdirAll(filepath.Join(dir, "codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	file := filepath.Join(dir, "codex", "one.json")
	if err := os.WriteFile(file, []byte(`{"tokens":{"access_token":"old-token","refresh_token":"r"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	h := &proxyHandler{cfg: &config{poolDir: dir}, pool: newPoolState(nil, false), registry: NewProviderRegistry(&CodexProvider{}, nil, nil)}
	h.reloadAccounts()
	if h.pool.count() != 1 {
		t.Fatalf("setup: loaded %d accounts, want 1", h.pool.count())
	}
	pw, err := newPoolWatcher(dir, "", h)
	if err != nil {
		t.Fatal(err)
	}
	defer pw.close()

	if err := writeAccountFile(file, []byte(`{"tokens":{"access_token":"new-token","refresh_token":"r"}}`)); err != nil {
		t.Fatal(err)
	}
	if !auditWaitFor(t, 5*time.Second, func() bool {
		for _, a := range h.pool.allAccounts() {
			a.mu.Lock()
			token := a.AccessToken
			a.mu.Unlock()
			if token == "new-token" {
				return true
			}
		}
		return false
	}) {
		t.Fatal("atomic credential save did not hot-reload within 5s (watcher missed rename event)")
	}
}

// TestAuditWatcherReloadsCredentialAddedToNewProviderDirectory
// BUG-AUDIT-101: provider subdirectories created after startup are never
// added to the fsnotify watch (newPoolWatcher only scans once), so credential
// files saved inside them do not hot-reload. The poolDir Create event fires
// for the directory itself but nothing watches its contents afterwards.
// Expected: adding pool/<new-provider>/one.json triggers a reload.
// Actual: no event is delivered; the account only appears after a manual
// /admin/reload or restart.
func TestAuditWatcherReloadsCredentialAddedToNewProviderDirectory(t *testing.T) {
	dir := t.TempDir()
	h := &proxyHandler{cfg: &config{poolDir: dir}, pool: newPoolState(nil, false), registry: NewProviderRegistry(&CodexProvider{}, nil, nil)}
	pw, err := newPoolWatcher(dir, "", h)
	if err != nil {
		t.Fatal(err)
	}
	defer pw.close()

	// Create the provider directory after startup; the Create event for the
	// directory itself triggers one (empty) reload after the debounce.
	if err := os.MkdirAll(filepath.Join(dir, "codex"), 0o755); err != nil {
		t.Fatal(err)
	}
	time.Sleep(1200 * time.Millisecond)

	// A credential file inside the new provider directory must hot-reload.
	file := filepath.Join(dir, "codex", "one.json")
	if err := os.WriteFile(file, []byte(`{"tokens":{"access_token":"late-token","refresh_token":"r"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !auditWaitFor(t, 5*time.Second, func() bool { return h.pool.count() == 1 }) {
		t.Fatalf("BUG-AUDIT-101: credential added to provider directory created after startup never hot-reloaded (pool=%d)", h.pool.count())
	}
}

// TestAuditAnalyticsGapSidecarConcurrentWritersNeverCorrupt
// BUG-AUDIT-102: persistActiveAccountingGapSidecar uses a fixed "<path>.tmp"
// name with no mutual exclusion around the write+rename. Concurrent failing
// requests both write the shared temp file; when both truncates land before
// both writes, the final sidecar mixes payloads from two writers and the
// JSON no longer unmarshals, silently discarding the active accounting gap
// on restart. Expected: the sidecar always parses as exactly one snapshot.
func TestAuditAnalyticsGapSidecarConcurrentWritersNeverCorrupt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gap.json")
	s := &usageStore{analyticsGapPath: path}
	big := &AccountingGap{StartedAt: time.Now().UTC(), Reason: strings.Repeat("x", 8192)}
	small := &AccountingGap{StartedAt: time.Now().UTC().Add(time.Second), Reason: "short"}

	for round := 0; round < 300; round++ {
		var wg sync.WaitGroup
		start := make(chan struct{})
		wg.Add(2)
		go func() { defer wg.Done(); <-start; s.persistActiveAccountingGapSidecar(big) }()
		go func() { defer wg.Done(); <-start; s.persistActiveAccountingGapSidecar(small) }()
		close(start)
		wg.Wait()

		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatalf("round %d: sidecar missing: %v", round, err)
		}
		var gap AccountingGap
		if err := json.Unmarshal(raw, &gap); err != nil {
			t.Fatalf("BUG-AUDIT-102: round %d corrupted sidecar (%d bytes): %v", round, len(raw), err)
		}
		if gap.Reason != big.Reason && gap.Reason != small.Reason {
			t.Fatalf("BUG-AUDIT-102: round %d mixed payloads: %q", round, gap.Reason)
		}
	}
}
