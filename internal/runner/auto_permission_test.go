package runner

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"codex-pool-proxy/internal/runnerproto"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// runAutoSession claims one auto permission session and returns the first
// session event the runner emits, plus whether it fetched agent credentials.
func runAutoSession(t *testing.T, allow bool) (runnerproto.Message, bool, string) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	var downloads atomic.Int32
	first := make(chan runnerproto.Message, 1)
	session := runnerproto.Session{ID: "5d1d9c3e-9f1a-4f0e-8a51-2f4b7c1d0e11", RunnerID: "0c6f0f8e-3c55-4c9e-9b8a-7d2f1e4a5b66", Agent: "claude", Model: "synthetic-model", PermissionMode: "auto", Workspace: runnerproto.Workspace{Mode: "ephemeral"}, State: "claimed"}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/config/claude/01234567890123456789012345678901" {
			downloads.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "synthetic-session-key"})
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		for {
			var m runnerproto.Message
			if wsjson.Read(ctx, conn, &m) != nil {
				return
			}
			switch m.Type {
			case "hello":
				pending := runnerproto.Frame("session.pending")
				pending.SessionID = session.ID
				if wsjson.Write(ctx, conn, runnerproto.Frame("hello.ack")) != nil || wsjson.Write(ctx, conn, pending) != nil {
					return
				}
			case "claim":
				ack := runnerproto.Frame("claim.ack")
				ack.SessionID = session.ID
				ack.Session = &session
				ack.CredentialPath = "/config/claude/01234567890123456789012345678901"
				if wsjson.Write(ctx, conn, ack) != nil {
					return
				}
			case "heartbeat":
				if wsjson.Write(ctx, conn, runnerproto.Frame("heartbeat.ack")) != nil {
					return
				}
			default:
				if m.SessionID == session.ID {
					select {
					case first <- m:
					default:
					}
				}
			}
		}
	}))
	defer server.Close()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	result := make(chan error, 1)
	go func() {
		result <- Run(ctx, Options{Config: Config{PoolURL: server.URL, RunnerID: session.RunnerID, Token: "pool-runner-" + session.RunnerID + ".synthetic"}, WorkspaceRoot: root, Executables: map[string]string{"claude": exe}, HTTPClient: server.Client(), AllowAutoPermissions: allow})
	}()
	var event runnerproto.Message
	select {
	case event = <-first:
	case err := <-result:
		t.Fatalf("runner stopped early: %v", err)
	case <-ctx.Done():
		t.Fatal("runner emitted no session event")
	}
	cancel()
	<-result
	return event, downloads.Load() > 0, filepath.Join(root, session.ID)
}

func TestRunnerRefusesAutoSessionsWithoutLocalOptIn(t *testing.T) {
	event, downloaded, workspace := runAutoSession(t, false)
	if event.Type != "process.exit" || event.ExitCode == 0 || event.Status != runnerproto.ExitAutoPermissionsDisabled {
		t.Fatalf("first event = %s exit=%d status=%q, want a failed process.exit explaining the refusal", event.Type, event.ExitCode, event.Status)
	}
	if downloaded {
		t.Fatal("refused auto session still fetched agent credentials")
	}
	if _, err := os.Stat(workspace); !os.IsNotExist(err) {
		t.Fatalf("refused auto session created its workspace: %v", err)
	}
}

func TestRunnerAcceptsAutoSessionsWithLocalOptIn(t *testing.T) {
	event, _, _ := runAutoSession(t, true)
	if event.Type != "workspace.status" || event.Status != "preparing" {
		t.Fatalf("first event = %s/%s, want workspace preparation", event.Type, event.Status)
	}
}

func TestLaunchConfigAllowAutoDefaultsOff(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "runner.json")
	base := map[string]any{"state": filepath.Join(root, "state.json"), "pool_url": "https://pool.example", "name": "desktop", "workspaces": filepath.Join(root, "workspaces"), "codex": filepath.Join(root, "codex.exe")}
	for _, allow := range []any{nil, true} {
		value := map[string]any{}
		for k, v := range base {
			value[k] = v
		}
		if allow != nil {
			value["allow_auto"] = allow
		}
		data, err := json.Marshal(value)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(path, data, 0600); err != nil {
			t.Fatal(err)
		}
		cfg, err := ReadLaunchConfig(path)
		if err != nil {
			t.Fatal(err)
		}
		if cfg.AllowAuto != (allow != nil) {
			t.Fatalf("allow_auto=%v parsed as %v", allow, cfg.AllowAuto)
		}
	}
}
