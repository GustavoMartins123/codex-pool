//go:build windows

package main

// Windows-only adversarial tests for atomic file replacement semantics.
// On POSIX, rename(2) over a file that another process holds open always
// succeeds. On Windows, MoveFileEx fails with ERROR_ACCESS_DENIED when the
// destination is open without FILE_SHARE_DELETE — exactly what antivirus
// scanners, backup agents, search indexers, and most editors do while
// briefly holding a credential file.

import (
	"os"
	"path/filepath"
	"syscall"
	"testing"
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

// TestAuditWriteFileAtomicReplacesFileHeldWithoutShareDelete
// BUG-AUDIT-001 (Windows-only): writeFileAtomic's os.Rename target cannot be
// replaced while any process holds the destination without
// FILE_SHARE_DELETE. Linux completes the same rename without error, so token
// refreshes and credential updates that race with an AV scan fail only on
// Windows, dropping the refreshed credential.
// Expected: the atomic replace succeeds (retry or replace-by-handle) and the
// file content is fully updated.
// Actual: os.Rename fails with "Access is denied." and the update is lost.
func TestAuditWriteFileAtomicReplacesFileHeldWithoutShareDelete(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credential.json")
	if err := os.WriteFile(path, []byte(`{"tokens":{"access_token":"old"}}`), 0o600); err != nil {
		t.Fatal(err)
	}
	handle, err := openAuditFileExclusive(path)
	if err != nil {
		t.Skipf("cannot simulate exclusive reader: %v", err)
	}
	defer syscall.CloseHandle(handle)

	if err := writeAccountFile(path, []byte(`{"tokens":{"access_token":"new"}}`)); err != nil {
		t.Fatalf("BUG-AUDIT-001: atomic credential replace failed while a non-sharing reader held the file: %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"tokens":{"access_token":"new"}}` {
		t.Fatalf("BUG-AUDIT-001: credential not updated: %s", raw)
	}
}

// TestAuditWriteFileAtomicToleratesGoReaderHoldingDestination
// BUG-AUDIT-001 (Windows-only, realistic internal variant): Go's os.Open
// holds files without FILE_SHARE_DELETE, so writeFileAtomic fails whenever
// the destination is being read by codex-pool itself — e.g. a token-refresh
// save racing the watcher's hot-reload readAccountFile of the same file, or
// a passport backup walking the pool directory. On Linux the same rename
// over an open descriptor always succeeds.
// Expected: the atomic replace succeeds and the content is fully updated.
// Actual: os.Rename fails with "Access is denied." and the update is lost.
func TestAuditWriteFileAtomicToleratesGoReaderHoldingDestination(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "credential.json")
	if err := os.WriteFile(path, []byte(`{"v":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	f, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer f.Close()

	if err := writeAccountFile(path, []byte(`{"v":2}`)); err != nil {
		t.Fatalf("BUG-AUDIT-001: atomic replace blocked by a plain Go reader (hot-reload/backup races fail on Windows): %v", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if string(raw) != `{"v":2}` {
		t.Fatalf("credential not updated: %s", raw)
	}
}
