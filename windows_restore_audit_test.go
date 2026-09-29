//go:build windows

package main

// BUG-AUDIT-105, Windows-realistic variant: a handle holding usage.duckdb
// open without FILE_SHARE_DELETE (indexer, antivirus, a stray duckdb client)
// makes the DuckDB rename fail with access denied AFTER the Bolt rename
// already succeeded. The restore must roll the Bolt swap back so the stores
// stay a matched pair. Unlike the cross-platform tests, no rename injection
// is needed — this is the actual production failure shape on Windows.

import (
	"strings"
	"syscall"
	"testing"
)

func TestAuditRestorePairedBackupRollsBackWhenDuckHeldOpen(t *testing.T) {
	manifest, boltPath, duckPath := buildPairedRestoreFixture(t)

	p, err := syscall.UTF16PtrFromString(duckPath)
	if err != nil {
		t.Fatal(err)
	}
	handle, err := syscall.CreateFile(p, syscall.GENERIC_READ, 0, nil, syscall.OPEN_EXISTING, syscall.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		t.Skipf("cannot hold the DuckDB file exclusively: %v", err)
	}
	defer syscall.CloseHandle(handle)

	if err := restorePairedBackup(manifest, boltPath, duckPath); err == nil {
		t.Fatal("restore unexpectedly succeeded while the DuckDB file was held open")
	}
	if !strings.Contains(err.Error(), "rolled back") {
		t.Fatalf("restore error does not report rollback: %v", err)
	}
	if value := readBoltProof(t, boltPath); value != "after" {
		t.Fatalf("BUG-AUDIT-105: torn restore — Bolt left at %q although the DuckDB swap was blocked", value)
	}
	assertNoRestoreLeftovers(t, boltPath, duckPath)
}
