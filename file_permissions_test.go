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

// A bind-mounted pool directory routinely arrives as 0755. The audit must
// tighten it in place instead of refusing to start, because the fix is safe;
// only the credential files hard-fail.
func TestVerifySensitiveFilePermissionsRemediatesDirectory(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("POSIX permission bits do not apply on Windows")
	}
	parent := t.TempDir()
	if err := os.Chmod(parent, 0o700); err != nil {
		t.Fatal(err)
	}
	dir := filepath.Join(parent, "pool")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Chmod(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	ok := filepath.Join(dir, "codex-ok.json")
	if err := os.WriteFile(ok, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}

	problems := verifySensitiveFilePermissions(dir)
	if len(problems) != 0 {
		t.Fatalf("a 0755 pool directory must be remediated, not reported: %v", problems)
	}
	info, err := os.Stat(dir)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o700 {
		t.Fatalf("pool directory mode = %o, want 700", info.Mode().Perm())
	}
	// Idempotent: a second audit on the tightened tree is clean.
	if problems := verifySensitiveFilePermissions(dir); len(problems) != 0 {
		t.Fatalf("tightened tree must be clean, got %v", problems)
	}
}
