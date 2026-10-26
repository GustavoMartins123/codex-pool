package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"

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

func TestCatalogRefreshUsesEtagsAndNotifiesOnlyCapabilityChanges(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("uses Unix scripts for executable replacement")
	}
	var mu sync.Mutex
	catalog := runnerproto.RunnerCatalog{Models: []runnerproto.AgentModel{{Agent: "claude", Model: "first"}}, CodexCatalog: `{"models":[]}`}
	catalog.Revision = runnerproto.ContentRevision(catalog)
	requests, unchanged := 0, 0
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		requests++
		if r.Header.Get("If-None-Match") == `"`+catalog.Revision+`"` {
			unchanged++
			w.WriteHeader(304)
			return
		}
		json.NewEncoder(w).Encode(catalog)
	}))
	defer server.Close()
	exe := filepath.Join(t.TempDir(), "claude")
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho one\n"), 0700); err != nil {
		t.Fatal(err)
	}
	d := &daemon{ctx: context.Background(), options: Options{WorkspaceRoot: t.TempDir(), Config: Config{PoolURL: server.URL, Token: "synthetic"}, HTTPClient: server.Client(), Executables: map[string]string{"claude": exe}}, catalogNotify: make(chan struct{}, 1)}
	if err := d.refreshModels(); err != nil {
		t.Fatal(err)
	}
	firstRevision, fingerprint := d.modelsRevision, d.agentFingerprint
	<-d.catalogNotify
	if err := d.refreshModels(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-d.catalogNotify:
		t.Fatal("unchanged catalog caused another update")
	default:
	}
	mu.Lock()
	if requests != 2 || unchanged != 1 {
		t.Errorf("requests=%d unchanged=%d", requests, unchanged)
	}
	mu.Unlock()
	if err := os.WriteFile(exe, []byte("#!/bin/sh\necho two-with-a-new-size\n"), 0700); err != nil {
		t.Fatal(err)
	}
	if err := d.refreshModels(); err != nil {
		t.Fatal(err)
	}
	if d.agentFingerprint == fingerprint {
		t.Fatal("executable replacement was ignored")
	}
	mu.Lock()
	catalog.Models = []runnerproto.AgentModel{{Agent: "claude", Model: "second"}}
	catalog.Revision = ""
	catalog.Revision = runnerproto.ContentRevision(catalog)
	mu.Unlock()
	if err := d.refreshModels(); err != nil {
		t.Fatal(err)
	}
	if d.modelsRevision == firstRevision || d.models[0].Model != "second" {
		t.Fatal("new source catalog was ignored")
	}
	select {
	case <-d.catalogNotify:
	default:
		t.Fatal("capability change was not queued for the pool")
	}
	if _, ok := d.catalogGenerations[firstRevision]; !ok {
		t.Fatal("in-flight sessions lost their earlier catalog generation")
	}
	if err := os.Remove(exe); err != nil {
		t.Fatal(err)
	}
	if err := d.refreshModels(); err != nil {
		t.Fatal(err)
	}
	if len(d.models) != 0 {
		t.Fatal("removed executable kept advertising models")
	}
}

func TestCatalogWatcherRecoversFromAnInitialFailure(t *testing.T) {
	var mu sync.Mutex
	fail := true
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		if fail {
			w.WriteHeader(503)
			return
		}
		json.NewEncoder(w).Encode(runnerproto.RunnerCatalog{Models: []runnerproto.AgentModel{{Agent: "claude", Model: "recovered"}}, CodexCatalog: `{"models":[]}`})
	}))
	defer server.Close()
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	d := &daemon{ctx: ctx, options: Options{WorkspaceRoot: t.TempDir(), Config: Config{PoolURL: server.URL}, HTTPClient: server.Client(), Executables: map[string]string{"claude": exe}, modelsRefreshInterval: 10 * time.Millisecond}, catalogNotify: make(chan struct{}, 1)}
	if err := d.refreshModels(); err == nil {
		t.Fatal("fixture did not fail the first request")
	}
	mu.Lock()
	fail = false
	mu.Unlock()
	done := make(chan struct{})
	go func() { d.watchModels(); close(done) }()
	select {
	case <-d.catalogNotify:
	case <-time.After(2 * time.Second):
		t.Fatal("watcher did not recover")
	}
	cancel()
	<-done
	d.mu.Lock()
	defer d.mu.Unlock()
	if len(d.models) != 1 || d.models[0].Model != "recovered" {
		t.Fatal("recovered catalog was not confirmed")
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
