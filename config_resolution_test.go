package main

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
)

// P1-02 regression: a config file that exists but fails to parse must stop
// startup, and CONFIG_PATH must drive the same file the watcher observes.
func TestConfigFileInvalidFailsStartup(t *testing.T) {
	if os.Getenv("CODEX_POOL_TEST_INVALID_CONFIG") == "1" {
		buildConfig()
		return
	}
	dir := t.TempDir()
	path := filepath.Join(dir, "config.toml")
	if err := os.WriteFile(path, []byte("debug = [unclosed"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("CONFIG_PATH", path)
	cmd := exec.Command(os.Args[0], "-test.run=TestConfigFileInvalidFailsStartup", "-test.v")
	cmd.Env = append(cmd.Environ(), "CODEX_POOL_TEST_INVALID_CONFIG=1", "CONFIG_PATH="+path)
	out, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("startup with broken config unexpectedly succeeded:\n%s", out)
	}
	output := string(out)
	want := "invalid config file"
	if !strings.Contains(output, want) || !strings.Contains(output, path) {
		t.Fatalf("startup with broken config must abort naming %q, got:\n%s", want, output)
	}
}

func TestConfigFilePathSingleResolution(t *testing.T) {
	t.Setenv("CONFIG_PATH", "")
	if got := configFilePath(); got != "config.toml" {
		t.Fatalf("default config path = %q", got)
	}
	t.Setenv("CONFIG_PATH", "/srv/pool/config.toml")
	if got := configFilePath(); got != "/srv/pool/config.toml" {
		t.Fatalf("override config path = %q", got)
	}
}
