package runner

import (
	"context"
	"encoding/json"
	"errors"
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

func TestDaemonReattachesWithoutRestartingOrRepeatingTurns(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	var connections, downloads, finished atomic.Int32
	var sequence atomic.Uint64
	var pendingApproval, pendingCommand atomic.Value
	commandKey, err := NewCommandKey()
	if err != nil {
		t.Fatal(err)
	}
	key, err := decodeCommandKey(commandKey)
	if err != nil {
		t.Fatal(err)
	}
	// A redelivered turn keeps the issue time it was signed with.
	issuedAt := map[string]int64{"turn-one": time.Now().UnixMilli(), "turn-two": time.Now().UnixMilli() + 1}
	completed := make(chan struct{})
	failures := make(chan error, 4)
	session := runnerproto.Session{ID: "11f168da-5cc4-415e-b25f-a41514a56812", RunnerID: "b4625b08-b9c3-4498-8a4c-81e408444ec3", Agent: "claude", Model: "synthetic-model", PermissionMode: "gated", Workspace: runnerproto.Workspace{Mode: "ephemeral"}, State: "claimed", TurnID: "turn-one"}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/config/claude/01234567890123456789012345678901" {
			downloads.Add(1)
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "synthetic-session-key"})
			return
		}
		if r.URL.Path != "/api/runner/ws" {
			http.NotFound(w, r)
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			failures <- err
			return
		}
		defer conn.CloseNow()
		connection := connections.Add(1)
		fail := func(err error) {
			select {
			case failures <- err:
			default:
			}
		}
		write := func(m runnerproto.Message) bool {
			if err := wsjson.Write(ctx, conn, m); err != nil {
				fail(err)
				return false
			}
			return true
		}
		start := func(turn string) bool {
			m := runnerproto.Frame("turn.start")
			m.SessionID = session.ID
			m.TurnID = turn
			m.Input = "synthetic"
			m.Settings = &runnerproto.TurnSettings{Model: session.Model}
			m.IssuedAt = issuedAt[turn]
			m.Signatures = map[string]string{runnerproto.CommandKeyID(key): runnerproto.SignCommand(key, runnerproto.TurnSigningInput(session.ID, m.IssuedAt, session.Agent, session.PermissionMode, session.Workspace, m.Input))}
			return write(m)
		}
		for {
			var m runnerproto.Message
			if err := wsjson.Read(ctx, conn, &m); err != nil {
				if ctx.Err() == nil {
					fail(err)
				}
				return
			}
			switch m.Type {
			case "hello":
				if !write(runnerproto.Frame("hello.ack")) {
					return
				}
				pending := runnerproto.Frame("session.pending")
				pending.SessionID = session.ID
				if !write(pending) {
					return
				}
			case "claim":
				ack := runnerproto.Frame("claim.ack")
				ack.SessionID = session.ID
				copy := session
				if connection > 1 {
					copy.State = "waiting_approval"
				}
				ack.Session = &copy
				ack.Sequence = sequence.Load()
				ack.CredentialPath = "/config/claude/01234567890123456789012345678901"
				if !write(ack) {
					return
				}
				if connection > 1 {
					decision := runnerproto.Frame("approval.decision")
					decision.SessionID = session.ID
					decision.ApprovalID = pendingApproval.Load().(string)
					decision.Decision = "approve"
					decision.IssuedAt = time.Now().UnixMilli()
					decision.Signatures = map[string]string{runnerproto.CommandKeyID(key): runnerproto.SignCommand(key, runnerproto.ApprovalSigningInput(session.ID, decision.ApprovalID, decision.Decision, decision.IssuedAt, pendingCommand.Load().(string)))}
					if !write(decision) {
						return
					}
				}
			case "workspace.status", "turn.started", "turn.event", "approval.request", "turn.finished", "process.exit", "agent.checkpoint":
				if m.Sequence != sequence.Load()+1 {
					fail(errors.New("unexpected event sequence"))
					return
				}
				sequence.Store(m.Sequence)
				if connection == 1 && m.Type == "approval.request" { // The event committed, but its ACK is lost.
					pendingApproval.Store(m.ApprovalID)
					pendingCommand.Store(m.Command)
					conn.CloseNow()
					return
				}
				ack := runnerproto.Frame("event.ack")
				ack.SessionID = session.ID
				ack.Sequence = m.Sequence
				if !write(ack) {
					return
				}
				if m.Type == "workspace.status" && m.Status == "ready" {
					if !start("turn-one") {
						return
					}
				}
				if m.Type == "approval.request" {
					decision := runnerproto.Frame("approval.decision")
					decision.SessionID = session.ID
					decision.ApprovalID = m.ApprovalID
					decision.Decision = "deny"
					if !write(decision) {
						return
					}
				}
				if m.Type == "turn.finished" {
					count := finished.Add(1)
					if count == 1 {
						if !start("turn-one") || !start("turn-two") {
							return
						}
					} else if count == 2 {
						close := runnerproto.Frame("session.close")
						close.SessionID = session.ID
						if !write(close) {
							return
						}
					} else {
						fail(errors.New("turn executed twice"))
						return
					}
				}
				if m.Type == "process.exit" {
					if m.Checkpoint == nil || !m.Checkpoint.Ready || m.Status == "checkpoint_failed" {
						fail(errors.New("native checkpoint was not persisted"))
						return
					}
					closeMessage := runnerproto.Frame("session.close")
					closeMessage.SessionID = session.ID
					_ = write(closeMessage)
					close(completed)
					return
				}
			case "heartbeat":
				if !write(runnerproto.Frame("heartbeat.ack")) {
					return
				}
			default:
				fail(errors.New("unexpected runner message"))
				return
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
		result <- Run(ctx, Options{Config: Config{PoolURL: server.URL, RunnerID: session.RunnerID, Token: "pool-runner-" + session.RunnerID + ".synthetic", CommandKey: commandKey}, WorkspaceRoot: root, Executables: map[string]string{"claude": exe}, HTTPClient: server.Client()})
	}()
	select {
	case <-completed:
	case err := <-failures:
		t.Fatal(err)
	case err := <-result:
		t.Fatalf("runner stopped early: %v", err)
	case <-ctx.Done():
		t.Fatal("runner integration timed out")
	}
	cancel()
	if err := <-result; err != nil {
		t.Fatal(err)
	}
	if connections.Load() != 2 || downloads.Load() != 1 || finished.Load() != 2 {
		t.Fatalf("connections=%d credentials=%d turns=%d", connections.Load(), downloads.Load(), finished.Load())
	}
	if _, err := os.Stat(filepath.Join(root, session.ID)); err != nil {
		t.Fatal("workspace required for resume was removed")
	}
}
