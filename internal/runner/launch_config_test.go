package runner

import (
	"os"
	"path/filepath"
	"testing"
)

func TestLaunchConfigRejectsSecretsAndRelativePaths(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "runner.json")
	valid := `{"state":"C:\\runner\\state.json","pool_url":"https://pool.example","name":"desktop","workspaces":"C:\\runner\\workspaces","codex":"C:\\Users\\me\\codex.exe","claude":"","allow_paths":["C:\\repos"],"labels":["windows"]}`
	if err := os.WriteFile(path, []byte(valid), 0600); err != nil {
		t.Fatal(err)
	}
	if _, err := ReadLaunchConfig(path); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{
		`{"state":"state.json","pool_url":"https://pool.example","name":"desktop","workspaces":"C:\\work","codex":"C:\\codex.exe"}`,
		`{"state":"C:\\state.json","pool_url":"https://pool.example","name":"desktop","workspaces":"C:\\work","codex":"C:\\codex.exe","token":"secret"}`,
		`{"state":"C:\\state.json","pool_url":"http://pool.example","name":"desktop","workspaces":"C:\\work","codex":"C:\\codex.exe"}`,
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
	value := `{"state":"` + filepath.Join(root, "state.json") + `","pool_url":"https://pool.example","name":"desktop","workspaces":"` + filepath.Join(root, "workspaces") + `","codex":"` + filepath.Join(root, "codex.exe") + `"}`
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
