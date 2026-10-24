package runner

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLaunchConfigRejectsSecretsAndRelativePaths(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "runner.json")
	escape := func(value string) string { return strings.ReplaceAll(value, `\`, `\\`) }
	state := escape(filepath.Join(root, "state.json"))
	workspaces := escape(filepath.Join(root, "workspaces"))
	codex := escape(filepath.Join(root, "codex.exe"))
	allowPath := escape(filepath.Join(root, "repos"))
	valid := `{"state":"` + state + `","pool_url":"https://pool.example","name":"desktop","workspaces":"` + workspaces + `","codex":"` + codex + `","claude":"","allow_paths":["` + allowPath + `"],"labels":["windows"]}`
	if err := os.WriteFile(path, []byte(valid), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLaunchConfig(path); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{
		`{"state":"state.json","pool_url":"https://pool.example","name":"desktop","workspaces":"` + workspaces + `","codex":"` + codex + `"}`,
		`{"state":"` + state + `","pool_url":"https://pool.example","name":"desktop","workspaces":"` + workspaces + `","codex":"` + codex + `","token":"secret"}`,
		`{"state":"` + state + `","pool_url":"http://pool.example","name":"desktop","workspaces":"` + workspaces + `","codex":"` + codex + `"}`,
	} {
		if err := os.WriteFile(path, []byte(value), 0600); err != nil {
			t.Fatal(err)
		}
		if _, err := ReadLaunchConfig(path); err == nil {
			t.Fatal("invalid launch config accepted")
		}
	}
}

func TestLaunchConfigDoesNotReadRunnerCredential(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "runner.json")
	escape := func(value string) string { return strings.ReplaceAll(value, `\`, `\\`) }
	value := `{"state":"` + escape(filepath.Join(root, "state.json")) + `","pool_url":"https://pool.example","name":"desktop","workspaces":"` + escape(filepath.Join(root, "workspaces")) + `","codex":"` + escape(filepath.Join(root, "codex.exe")) + `"}`
	if err := os.WriteFile(path, []byte(value), 0600); err != nil {
		t.Fatal(err)
	}
	cfg, err := ReadLaunchConfig(path)
	if err != nil {
		t.Fatal(err)
	}
	if cfg.State == "" || cfg.PoolURL == "" || cfg.Codex == "" {
		t.Fatal("launch config lost canonical fields")
	}
}
