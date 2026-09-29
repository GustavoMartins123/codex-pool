package main

// Restart/persistence adversarial tests: corrupted, truncated, and locked
// database files must fail gracefully (error, not panic or hard crash),
// because DuckDB and Bolt sit behind CGO/native code paths where a crash
// takes the whole proxy down.
//
// Truncated-Bolt repros MUST run in a subprocess: bbolt dereferences page
// pointers off its mmap without bounds checks, so a truncated store raises
// SIGBUS — a fatal, unrecoverable fault that would kill the entire
// `go test` process. The parent test never opens the corrupted store
// in-process; it observes the helper's exit status and output.

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

const (
	boltCorruptHelperEnv = "CODEX_POOL_TEST_BOLT_PATH"
	boltSmallSweepHelper = "CODEX_POOL_TEST_BOLT_SMALL_SWEEP"
)

// TestAuditUsageStoreTruncatedBoltFailsGracefully
// BUG-AUDIT-109 regression (cross-platform): a proxy.db truncated by a crash
// between write and flush must produce a clean, actionable error at startup —
// never a SIGBUS or a raw panic that kills the process on every restart until
// an operator repairs the file by hand.
//
// The corrupted store is only ever opened by the helper subprocess; the
// parent asserts the helper survives with a corruption error.
func TestAuditUsageStoreTruncatedBoltFailsGracefully(t *testing.T) {
	// Helper branch: this process was re-executed by the parent to open the
	// corrupted store. It must fail the clean way (error), never crash.
	if path := os.Getenv(boltCorruptHelperEnv); path != "" {
		_, err := newUsageStore(path, 30)
		if err == nil {
			t.Fatal("opening a truncated Bolt store unexpectedly succeeded")
		}
		if !errors.Is(err, ErrBoltCorrupt) {
			t.Fatalf("expected a corruption error, got: %v", err)
		}
		if !strings.Contains(err.Error(), "passport backup") {
			t.Fatalf("corruption error lacks remediation hint: %v", err)
		}
		return
	}
	// Small-file sweep helper: a proxy.db of 1..27 bytes is shorter than the
	// meta-page header probe; the guard must reject it with ErrBoltCorrupt
	// and never slice out of range. Sizes around the meta decode boundaries
	// (page header 16, meta struct 64, both plus one) cover every partial
	// read shape.
	if dir := os.Getenv(boltSmallSweepHelper); dir != "" {
		for size := 1; size <= 27; size++ {
			path := filepath.Join(dir, "small.db")
			if err := os.WriteFile(path, make([]byte, size), 0o600); err != nil {
				t.Fatal(err)
			}
			store, err := newUsageStore(path, 30)
			if err == nil {
				store.Close()
				t.Fatalf("%d-byte file opened a store unexpectedly", size)
			}
			if !errors.Is(err, ErrBoltCorrupt) {
				t.Fatalf("%d-byte file: expected ErrBoltCorrupt, got: %v", size, err)
			}
			t.Logf("size %d: %v", size, err)
		}
		for _, size := range []int{28, 79, 80, 81, 95, 96, 4095, 4096} {
			path := filepath.Join(dir, "small.db")
			payload := make([]byte, size)
			// Deterministic non-zero content: not a valid meta page, but a
			// plausible torn header.
			for i := range payload {
				payload[i] = byte(i * 7)
			}
			if err := os.WriteFile(path, payload, 0o600); err != nil {
				t.Fatal(err)
			}
			if store, err := newUsageStore(path, 30); err == nil {
				store.Close()
				t.Fatalf("%d-byte garbage file opened a store unexpectedly", size)
			}
			t.Logf("size %d rejected", size)
		}
		return
	}

	// Parent branch: build the corrupted store, then re-exec this same test
	// in a subprocess. A SIGBUS or panic in the helper shows up as a non-zero
	// exit with signal output; the parent and the rest of the suite survive.
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
	if info.Size() < 2*4096 {
		t.Skipf("store too small to truncate meaningfully: %d bytes", info.Size())
	}
	for _, cut := range []int64{info.Size() / 2, info.Size() - 4096, info.Size() - 100} {
		if cut <= 0 {
			continue
		}
		if err := os.Truncate(path, cut); err != nil {
			t.Fatal(err)
		}
		out, err := runBoltCorruptHelper(t, path)
		if err != nil {
			t.Fatalf("BUG-AUDIT-109: opening a store truncated to %d bytes crashed the process instead of failing cleanly: %v\n%s", cut, err, out)
		}
	}

	// Small-file sweep in one helper subprocess: any panic slicing short
	// headers shows up as a child crash with the offending size as the last
	// logged line.
	sweepDir := filepath.Join(dir, "sweep")
	if err := os.Mkdir(sweepDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if out, err := runSmallSweepHelper(t, sweepDir); err != nil {
		t.Fatalf("BUG-AUDIT-109: small-file probe crashed the helper (last size attempted is the last line): %v\n%s", err, out)
	}
}

func runBoltCorruptHelper(t *testing.T, path string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestAuditUsageStoreTruncatedBoltFailsGracefully", "-test.timeout=2m", "-test.v")
	cmd.Env = append(os.Environ(), boltCorruptHelperEnv+"="+path)
	return cmd.CombinedOutput()
}

func runSmallSweepHelper(t *testing.T, dir string) ([]byte, error) {
	t.Helper()
	cmd := exec.Command(os.Args[0], "-test.run=TestAuditUsageStoreTruncatedBoltFailsGracefully", "-test.timeout=2m", "-test.v")
	cmd.Env = append(os.Environ(), boltSmallSweepHelper+"="+dir)
	return cmd.CombinedOutput()
}

// TestAuditUsageStoreCorruptBoltVariantsFailCleanly covers corruption shapes
// that bbolt rejects before any unsafe dereference, so they can run
// in-process: random garbage, tiny non-bolt files, and empty headers.
func TestAuditUsageStoreCorruptBoltVariantsFailCleanly(t *testing.T) {
	dir := t.TempDir()
	cases := map[string][]byte{
		"garbage":      []byte(strings.Repeat("not-a-bolt-database! ", 400)),
		"empty-header": make([]byte, 128),
		"torn-meta":    append(bytesRepeat(0x00, 16), bytesRepeat(0xff, 4096-16)...),
	}
	for name, payload := range cases {
		path := filepath.Join(dir, name+".db")
		if err := os.WriteFile(path, payload, 0o600); err != nil {
			t.Fatal(err)
		}
		store, err := newUsageStore(path, 30)
		if err == nil {
			store.Close()
			t.Fatalf("%s: opening a corrupt Bolt store unexpectedly succeeded", name)
		}
	}
}

func bytesRepeat(b byte, n int) []byte {
	out := make([]byte, n)
	for i := range out {
		out[i] = b
	}
	return out
}

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
