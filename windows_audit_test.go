package main

// Adversarial Windows audit tests. These target hidden POSIX assumptions in
// the filesystem, atomic-write, and watcher layers. Tests that intentionally
// fail against current production code carry a BUG-AUDIT-XXX marker in their
// failure message and are listed in docs/windows-audit-report.md.

import (
	"encoding/json"
	"errors"
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

// watchlistContains reports whether the watcher currently watches path.
func watchlistContains(pw *poolWatcher, path string) bool {
	for _, existing := range pw.watcher.WatchList() {
		if pathsEqual(existing, path) {
			return true
		}
	}
	return false
}

// TestAuditWatcherReloadsCredentialAddedToNewProviderDirectory
// BUG-AUDIT-101 regression (cross-platform): provider subdirectories created
// after startup must be added to the fsnotify watch, so credential files
// saved inside them hot-reload. The test synchronizes on the watch list
// (deterministic) instead of sleeping past the debounce.
func TestAuditWatcherReloadsCredentialAddedToNewProviderDirectory(t *testing.T) {
	dir := t.TempDir()
	h := &proxyHandler{cfg: &config{poolDir: dir}, pool: newPoolState(nil, false), registry: NewProviderRegistry(&CodexProvider{}, nil, nil)}
	pw, err := newPoolWatcher(dir, "", h)
	if err != nil {
		t.Fatal(err)
	}
	defer pw.close()

	// Create the provider directory after startup. The watch must be armed
	// before the credential file is written, otherwise the test would race
	// on whether the Create or the directory scan saw it first.
	providerDir := filepath.Join(dir, "codex")
	if err := os.MkdirAll(providerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if !auditWaitFor(t, 5*time.Second, func() bool { return watchlistContains(pw, providerDir) }) {
		t.Fatal("BUG-AUDIT-101: provider directory created after startup was never added to the watch")
	}

	// A credential file inside the new provider directory must hot-reload.
	file := filepath.Join(providerDir, "one.json")
	if err := os.WriteFile(file, []byte(`{"tokens":{"access_token":"late-token","refresh_token":"r"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !auditWaitFor(t, 5*time.Second, func() bool { return h.pool.count() == 1 }) {
		t.Fatalf("BUG-AUDIT-101: credential added to provider directory created after startup never hot-reloaded (pool=%d)", h.pool.count())
	}
}

// TestAuditWatcherRewatchesRenamedAndRecreatedProviderDirectory covers the
// rename/recreate cycle of a provider directory: the stale watch must be
// dropped and re-armed, so credentials written into the recreated directory
// still hot-reload.
func TestAuditWatcherRewatchesRenamedAndRecreatedProviderDirectory(t *testing.T) {
	dir := t.TempDir()
	h := &proxyHandler{cfg: &config{poolDir: dir}, pool: newPoolState(nil, false), registry: NewProviderRegistry(&CodexProvider{}, nil, nil)}
	pw, err := newPoolWatcher(dir, "", h)
	if err != nil {
		t.Fatal(err)
	}
	defer pw.close()

	providerDir := filepath.Join(dir, "codex")
	if err := os.MkdirAll(providerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if !auditWaitFor(t, 5*time.Second, func() bool { return watchlistContains(pw, providerDir) }) {
		t.Fatal("setup: initial provider directory not watched")
	}

	// Rename the provider directory away, then recreate it.
	if err := os.Rename(providerDir, filepath.Join(dir, "codex-old")); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(providerDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if !auditWaitFor(t, 5*time.Second, func() bool { return watchlistContains(pw, providerDir) }) {
		t.Fatal("BUG-AUDIT-101: recreated provider directory not re-watched")
	}

	file := filepath.Join(providerDir, "two.json")
	if err := os.WriteFile(file, []byte(`{"tokens":{"access_token":"recreated-token","refresh_token":"r"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if !auditWaitFor(t, 5*time.Second, func() bool { return h.pool.count() == 1 }) {
		t.Fatalf("BUG-AUDIT-101: credential in recreated provider directory never hot-reloaded (pool=%d)", h.pool.count())
	}
}

// TestAuditAnalyticsGapSidecarLifecycleNeverCorrupts
// BUG-AUDIT-102 regression (cross-platform), driven through the real
// lifecycle. The original repro called persistActiveAccountingGapSidecar
// directly with two arbitrary payloads — production cannot race two
// arbitrary payloads while a single gap is active (all snapshots share the
// first gap), but it CAN race a close against a delayed open-write, or a
// close+reopen cycle against a straggling writer, which mixed payloads in
// the shared ".tmp" file and silently discarded the accounting gap at
// restart.
//
// Two real paths are exercised:
//  1. recordReliably against a failing (closed) Bolt store — many
//     concurrent failed records all open/persist the same gap;
//  2. concurrent openAccountingGap/closeAccountingGap cycles with distinct
//     timestamps — close must remove the sidecar and no writer may leave a
//     torn or stale-behind file behind.
func TestAuditAnalyticsGapSidecarLifecycleNeverCorrupts(t *testing.T) {
	s := testUsageStore(t)
	// Closing the Bolt handle makes recordReliably fail on every call,
	// which is exactly the production trigger for openAccountingGap.
	if err := s.db.Close(); err != nil {
		t.Fatal(err)
	}

	usage := RequestUsage{Timestamp: time.Now().UTC(), AccountID: "gap", AccountType: AccountTypeCodex, UserID: "p1", ProxyRequestID: "req-gap"}
	var wg sync.WaitGroup
	start := make(chan struct{})
	for g := 0; g < 8; g++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			for i := 0; i < 25; i++ {
				_ = s.recordReliably(usage, 0)
			}
		}()
	}
	close(start)
	wg.Wait()

	raw, err := os.ReadFile(s.analyticsGapPath)
	if err != nil {
		t.Fatalf("active gap sidecar missing after failed records: %v", err)
	}
	var gap AccountingGap
	if err := json.Unmarshal(raw, &gap); err != nil {
		t.Fatalf("BUG-AUDIT-102: sidecar corrupted by concurrent failed records (%d bytes): %v", len(raw), err)
	}
	if gap.EndedAt != nil {
		t.Fatalf("active gap sidecar unexpectedly closed: %s", raw)
	}
}

// TestAuditAnalyticsGapSidecarOpenCloseRaceNeverCorrupts hammers the real
// open/close lifecycle with distinct timestamps so that, at any moment the
// sidecar exists, it parses as exactly one coherent snapshot and never
// mixes payloads from two writers.
func TestAuditAnalyticsGapSidecarOpenCloseRaceNeverCorrupts(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "proxy.db")
	s, err := newUsageStore(path, 30)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })

	var wg sync.WaitGroup
	start := make(chan struct{})
	for g := 0; g < 6; g++ {
		wg.Add(1)
		go func(goroutine int) {
			defer wg.Done()
			<-start
			for i := 0; i < 40; i++ {
				at := time.Now().UTC().Add(time.Duration(goroutine*100+i) * time.Millisecond)
				switch (goroutine + i) % 3 {
				case 0, 1:
					s.openAccountingGap(at, errors.New("synthetic write failure "+at.Format(time.RFC3339Nano)))
				case 2:
					s.closeAccountingGap(at)
				}
			}
		}(g)
	}
	close(start)
	wg.Wait()

	// Final state: either no sidecar (last lifecycle action closed the gap
	// cleanly) or one coherent active-gap snapshot. Anything else — parse
	// failure, mixed payloads, an "ended" gap left in the sidecar — is
	// corruption.
	raw, err := os.ReadFile(s.analyticsGapPath)
	if os.IsNotExist(err) {
		return
	}
	if err != nil {
		t.Fatal(err)
	}
	var gap AccountingGap
	if err := json.Unmarshal(raw, &gap); err != nil {
		t.Fatalf("BUG-AUDIT-102: sidecar corrupted by open/close race (%d bytes): %v", len(raw), err)
	}
	if gap.StartedAt.IsZero() {
		t.Fatalf("sidecar snapshot has no start time: %s", raw)
	}
	// The sidecar payload is always an ACTIVE gap (EndedAt is only set in
	// the closed snapshot persisted to Bolt, never in the sidecar).
	if gap.EndedAt != nil {
		t.Fatalf("BUG-AUDIT-102: sidecar holds a closed snapshot — stale writer resurrected a closed gap: %s", raw)
	}
}

// TestAuditAnalyticsGapSidecarDirectWritersNeverCorrupt is the unit-level
// guard for the sidecar writer itself: two writers with different payloads
// racing must never produce a mixed file. (The lifecycle races above are the
// production-reachable paths; this pins the writer primitive.)
func TestAuditAnalyticsGapSidecarDirectWritersNeverCorrupt(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "gap.json")
	s := &usageStore{analyticsGapPath: path}
	big := &AccountingGap{StartedAt: time.Now().UTC(), Reason: strings.Repeat("x", 8192)}
	small := &AccountingGap{StartedAt: time.Now().UTC().Add(time.Second), Reason: "short"}

	for round := 0; round < 100; round++ {
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
	// No staging temp files may survive.
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 1 {
		t.Fatalf("leftover staging files: %v", entries)
	}
}
