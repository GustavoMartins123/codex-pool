package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"codex-pool-proxy/internal/runnerproto"
)

func TestRunnerPreparesThePoolCatalogWithoutTheSiteSetup(t *testing.T) {
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	catalog := runnerproto.RunnerCatalog{
		Models: []runnerproto.AgentModel{
			{Agent: "codex", Model: "pool-only-model", ReasoningEfforts: []string{"low", "high", "max"}, DefaultReasoningEffort: "max"},
			{Agent: "codex", Model: "missing-model"},
			{Agent: "claude", Model: "claude-pool", ReasoningEfforts: []string{"high"}},
		},
		CodexCatalog: `{"models":[{"slug":"pool-only-model","supported_reasoning_levels":[{"effort":"low"},{"effort":"high"},{"effort":"max"}]}]}`,
	}
	options := Options{WorkspaceRoot: root, Executables: map[string]string{"codex": exe}, Config: Config{PoolURL: "https://pool.example"}}
	models, err := prepareAgentModels(context.Background(), options, catalog)
	if err != nil {
		t.Fatal(err)
	}
	want := []runnerproto.AgentModel{{Agent: "codex", Model: "pool-only-model", ReasoningEfforts: []string{"low", "high"}}}
	if !reflect.DeepEqual(models, want) {
		t.Fatalf("confirmed models = %+v, want %+v", models, want)
	}
	entries, _ := os.ReadDir(root)
	if len(entries) != 0 {
		t.Fatalf("probe left files: %v", entries)
	}
	control := t.TempDir()
	if err := saveCodexCatalog(control, catalog.CodexCatalog); err != nil {
		t.Fatal(err)
	}
	command := codexCommand(exe, root, control, "https://pool.example", "session-token", "gated")
	wanted := "model_catalog_json="
	found := false
	for _, arg := range command.Args {
		if len(arg) >= len(wanted) && arg[:len(wanted)] == wanted {
			found = true
		}
	}
	if !found {
		t.Fatal("session does not use the prepared model catalog")
	}
	if _, err := os.Stat(filepath.Join(control, "model_catalog.json")); err != nil {
		t.Fatal(err)
	}
}

func TestRunnerCatalogUsesOnlyTheRunnerCredential(t *testing.T) {
	for _, status := range []int{200, 401, 302} {
		t.Run(http.StatusText(status), func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.URL.Path != "/api/runner/catalog" || r.Header.Get("Authorization") != "Bearer runner-token" {
					t.Errorf("unexpected request %s", r.URL.Path)
				}
				w.Header().Set("Location", "/unexpected")
				w.WriteHeader(status)
				json.NewEncoder(w).Encode(runnerproto.RunnerCatalog{Models: []runnerproto.AgentModel{}, CodexCatalog: `{"models":[]}`})
			}))
			defer server.Close()
			_, err := fetchRunnerCatalog(context.Background(), Options{Config: Config{PoolURL: server.URL, Token: "runner-token"}, HTTPClient: server.Client()})
			if (err == nil) != (status == 200) {
				t.Fatalf("HTTP %d: %v", status, err)
			}
		})
	}
}
