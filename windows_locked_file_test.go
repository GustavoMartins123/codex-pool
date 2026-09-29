//go:build windows

package main

// Windows-only adversarial tests for atomic file replacement semantics.
// On POSIX, rename(2) over a file that another process holds open always
// succeeds. On Windows, MoveFileEx fails with ERROR_ACCESS_DENIED when the
// destination is open without FILE_SHARE_DELETE — exactly what antivirus
// scanners, backup agents, search indexers, and most editors do while
// briefly holding a credential file.
//
// The tests below model TRANSIENT locks: the holder releases the file after
// the writer has observably hit the lock, and the writer must then succeed
// via its bounded retry. A permanent holder is also covered — the writer
// must give up with an error inside the retry budget instead of hanging.

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
	"time"
)

// openAuditFileExclusive opens path for reading with no share modes,
// simulating an antivirus/indexer scan in progress.
func openAuditFileExclusive(path string) (syscall.Handle, error) {
	p, err := syscall.UTF16PtrFromString(path)
	if err != nil {
		return 0, err
	}
	return syscall.CreateFile(p, syscall.GENERIC_READ, 0, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
}

// runWriterUnderTransientLock runs write (an atomic replace) while a handle
// holds the destination open without FILE_SHARE_DELETE. The handle is
// released only after the writer has hit the lock at least once, proving the
// writer survives the transient window instead of failing fast or hanging.
func runWriterUnderTransientLock(t *testing.T, hold func() (release func()), write func() error) {
	t.Helper()
	retried := make(chan struct{}, 32)
	previous := writeFileAtomicRetryHook
	writeFileAtomicRetryHook = func() {
		select {
		case retried <- struct{}{}:
		default:
		}
	}
	t.Cleanup(func() { writeFileAtomicRetryHook = previous })

	release := hold()
	type outcome struct{ err error }
	done := make(chan outcome, 1)
	go func() { done <- outcome{write()} }()

	select {
	case <-retried:
		// The writer observed the lock; free it so the retry can succeed.
	case out := <-done:
		t.Fatalf("writer finished before observing the lock — test proves nothing (err=%v)", out.err)
	case <-time.After(3 * time.Second):
		t.Fatal("writer never retried against the locked destination")
	}
	release()

	select {
	case out := <-done:
		if out.err != nil {
			t.Fatalf("BUG-AUDIT-001: atomic replace failed against a transient lock: %v", out.err)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("writer did not complete after the transient lock was released")
	}
}

func TestAuditWriteFileAtomicSurvivesTransientExclusiveHolder(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credential.json")
	if err := os.WriteFile(path, []byte(`{"tokens":{"access_token":"old"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runWriterUnderTransientLock(t,
		func() func() {
			handle, err := openAuditFileExclusive(path)
			if err != nil {
				t.Skipf("cannot simulate exclusive reader: %v", err)
			}
			return func() { syscall.CloseHandle(handle) }
		},
		func() error {
			return writeAccountFile(path, []byte(`{"tokens":{"access_token":"new"}}`))
		})
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"tokens":{"access_token":"new"}}` {
		t.Fatalf("BUG-AUDIT-001: credential not updated after transient lock: %s", raw)
	}
}

// TestAuditWriteFileAtomicSurvivesTransientGoReader verifies the realistic
// internal variant: Go's os.Open never requests FILE_SHARE_DELETE, so the
// destination being read by codex-pool itself (watcher hot-reload, backup
// walk) blocks the rename exactly like an external scanner.
func TestAuditWriteFileAtomicSurvivesTransientGoReader(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credential.json")
	if err := os.WriteFile(path, []byte(`{"v":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	runWriterUnderTransientLock(t,
		func() func() {
			f, err := os.Open(path)
			if err != nil {
				t.Fatal(err)
			}
			return func() { f.Close() }
		},
		func() error {
			return writeAccountFile(path, []byte(`{"v":2}`))
		})
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"v":2}` {
		t.Fatalf("credential not updated after transient reader released: %s", raw)
	}
}

// TestAuditWriteFileAtomicGivesUpOnPermanentLockWithinBudget proves the
// retry is bounded: a holder that never releases must produce an error
// inside the total retry budget, not an unbounded hang.
func TestAuditWriteFileAtomicGivesUpOnPermanentLockWithinBudget(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credential.json")
	if err := os.WriteFile(path, []byte(`{"v":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	handle, err := openAuditFileExclusive(path)
	if err != nil {
		t.Skipf("cannot simulate exclusive reader: %v", err)
	}
	defer syscall.CloseHandle(handle)

	budget := time.Duration(0)
	for _, d := range renameRetryDelays {
		budget += d
	}
	deadline := time.Now().Add(budget + 5*time.Second)
	writeErr := make(chan error, 1)
	go func() { writeErr <- writeAccountFile(path, []byte(`{"v":2}`)) }()
	select {
	case err := <-writeErr:
		if err == nil {
			t.Fatal("replace unexpectedly succeeded while the destination was permanently locked")
		}
	case <-time.After(time.Until(deadline)):
		t.Fatal("writer exceeded the retry budget against a permanent lock")
	}
}
