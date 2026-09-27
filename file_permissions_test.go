package main

import (
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestVerifySensitiveFilePermissions(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits do not apply on Windows")
	}
	dir := t.TempDir()
	// t.TempDir's own mode varies by platform/umask; the audit must judge
	// only what the test sets up explicitly.
	if err := os.Chmod(dir, 0o700); err != nil {
		t.Fatal(err)
	}

	strict := filepath.Join(dir, "strict.json")
	if err := os.WriteFile(strict, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	if problems := verifySensitiveFilePermissions(dir); len(problems) != 0 {
		t.Fatalf("0600 file must not warn, got %v", problems)
	}

	lax := filepath.Join(dir, "lax.json")
	if err := os.WriteFile(lax, []byte("{}"), 0o644); err != nil {
		t.Fatal(err)
	}
	problems := verifySensitiveFilePermissions(dir)
	if len(problems) != 1 || problems[0] != lax {
		t.Fatalf("0644 credential file must warn, got %v", problems)
	}
}
