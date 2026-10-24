package runner

import (
	"context"
	"os"
	"path/filepath"
	"testing"
	"time"

	"codex-pool-proxy/internal/runnerproto"
)

func TestCheckpointRetainsWorkspaceAndRejectsForeignOrMissingState(t *testing.T) {
	for _, agent := range []string{"codex", "claude"} {
		t.Run(agent, func(t *testing.T) {
			root, err := filepath.EvalSymlinks(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			options := Options{WorkspaceRoot: root, Config: Config{PoolURL: "https://pool.example", RunnerID: "runner-one"}}
			session := runnerproto.Session{ID: "11111111-2222-4333-8444-555555555555", Agent: agent, Workspace: runnerproto.Workspace{Mode: "ephemeral"}}
			path, err := prepareWorkspace(context.Background(), root, nil, session)
			if err != nil {
				t.Fatal(err)
			}
			control, err := runtimePath(root, ".control-", session.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(filepath.Join(control, "sessions"), 0700); err != nil {
				t.Fatal(err)
			}
			nativeID := "native-thread"
			nativePath := filepath.Join(control, "sessions", nativeID+".jsonl")
			if err := os.WriteFile(nativePath, []byte("{\"turn\":1}\n"), 0600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(path, "code.txt"), []byte("retained workspace"), 0600); err != nil {
				t.Fatal(err)
			}
			session.Checkpoint, err = saveLocalCheckpoint(options, session, path, control, runnerproto.AgentCheckpoint{Version: 1, NativeSessionID: nativeID, NativeLogID: nativeID})
			if err != nil {
				t.Fatal(err)
			}
			session.ResumeRequested = true
			loaded, err := loadLocalCheckpoint(options, session)
			if err != nil || loaded.Path != path || loaded.NativeSessionID != nativeID {
				t.Fatalf("checkpoint load: %+v %v", loaded, err)
			}
			foreign := options
			foreign.Config.RunnerID = "runner-two"
			if _, err := loadLocalCheckpoint(foreign, session); err == nil {
				t.Fatal("foreign runner checkpoint accepted")
			}
			foreign = options
			foreign.Config.PoolURL = "https://other.example"
			if _, err := loadLocalCheckpoint(foreign, session); err == nil {
				t.Fatal("foreign pool checkpoint accepted")
			}
			if err := os.WriteFile(nativePath, []byte("corrupt native log"), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadLocalCheckpoint(options, session); err == nil {
				t.Fatal("corrupt native file accepted")
			}
			if err := os.Remove(nativePath); err != nil {
				t.Fatal(err)
			}
			if _, err := loadLocalCheckpoint(options, session); err == nil {
				t.Fatal("missing native state accepted")
			}
			if _, err := os.Stat(filepath.Join(path, "code.txt")); err != nil {
				t.Fatal("resume failure removed user files")
			}
		})
	}
}

func TestNativeAgentsResumeTheirOriginalSessionAfterProcessRestart(t *testing.T) {
	for _, agentKind := range []string{"codex", "claude"} {
		t.Run(agentKind, func(t *testing.T) {
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			workspace, control := t.TempDir(), t.TempDir()
			session := runnerproto.Session{ID: "11111111-2222-4333-8444-555555555555", Agent: agentKind, Model: "synthetic-model", PermissionMode: "gated"}
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			var native runnerproto.AgentCheckpoint
			for attempt := range 2 {
				events := make(chan runnerproto.Message, 32)
				exits := make(chan int, 1)
				emit := func(m runnerproto.Message) error { events <- m; return nil }
				onExit := func(code int) { exits <- code }
				var agent agentProcess
				if agentKind == "codex" {
					agent, err = startCodex(ctx, exe, workspace, control, "https://pool.example", "synthetic", session, emit, onExit)
				} else {
					agent, err = startClaude(ctx, exe, workspace, control, "https://pool.example", "synthetic", session, emit, onExit)
				}
				if err != nil {
					t.Fatal(err)
				}
				if attempt == 0 {
					native = agent.NativeCheckpoint()
				} else if native != agent.NativeCheckpoint() {
					t.Fatal("resume created another native session")
				}
				if err := agent.StartTurn("turn", "synthetic input"); err != nil {
					agent.Close()
					t.Fatal(err)
				}
				completed := false
				for !completed {
					select {
					case <-ctx.Done():
						agent.Close()
						t.Fatal("native resume test timed out")
					case m := <-events:
						if m.Type == "approval.request" {
							if err := agent.Decide(m.ApprovalID, "approve"); err != nil {
								agent.Close()
								t.Fatal(err)
							}
						}
						completed = m.Type == "turn.finished"
					}
				}
				agent.Close()
				select {
				case <-exits:
				case <-ctx.Done():
					t.Fatal("agent failed to close")
				}
				session.ResumeRequested = true
				checkpoint := native
				checkpoint.Ready = true
				session.Checkpoint = &checkpoint
			}
			raw, err := os.ReadFile(filepath.Join(control, "sessions", native.NativeLogID+".jsonl"))
			if err != nil || string(raw) != "2" {
				t.Fatalf("native turn state was not resumed: %q %v", raw, err)
			}
		})
	}
}
