//go:build !windows

package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"testing"
)

const setupScriptName = "setup.sh"

func setupTestCommand(script string, args ...string) *exec.Cmd {
	return exec.Command("bash", append([]string{script}, args...)...)
}

func checkSetupPermissions(t *testing.T, path string) {
	t.Helper()
	info, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0600 {
		t.Fatalf("environment mode: %o", info.Mode().Perm())
	}
}

func TestSetupRefusesSymbolicLink(t *testing.T) {
	dir, envPath := setupFixture(t)
	target := filepath.Join(dir, "existing")
	if err := os.WriteFile(target, []byte("unchanged"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, envPath); err != nil {
		t.Fatal(err)
	}
	if _, err := runSetup(t, dir); err == nil {
		t.Fatal("symbolic link accepted")
	}
	if readSetupEnv(t, target) != "unchanged" {
		t.Fatal("setup changed symlink target")
	}
}
