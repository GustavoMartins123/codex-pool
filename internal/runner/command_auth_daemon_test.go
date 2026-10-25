package runner

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"testing"
	"time"

	"codex-pool-proxy/internal/runnerproto"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
)

// runSignedTurn drives a paired runner through one turn and returns the first
// event it reports after the turn was offered.
func runSignedTurn(t *testing.T, sign func(key []byte, s runnerproto.Session, m *runnerproto.Message)) runnerproto.Message {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	encoded, err := NewCommandKey()
	if err != nil {
		t.Fatal(err)
	}
	key, _ := base64.RawURLEncoding.DecodeString(encoded)
	session := runnerproto.Session{ID: "3f2b1a0c-9d8e-4f7a-8b6c-5d4e3f2a1b0c", RunnerID: "6e5d4c3b-2a19-4807-b6a5-948372615040", Agent: "claude", Model: "synthetic-model", PermissionMode: "gated", Workspace: runnerproto.Workspace{Mode: "ephemeral"}, State: "claimed", TurnID: "turn-one"}
	reported := make(chan runnerproto.Message, 1)
	hello := make(chan runnerproto.Hello, 1)
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Path == "/config/claude/01234567890123456789012345678901" {
			w.Header().Set("Content-Type", "application/json")
			_ = json.NewEncoder(w).Encode(map[string]any{"access_token": "synthetic-session-key"})
			return
		}
		conn, err := websocket.Accept(w, r, nil)
		if err != nil {
			return
		}
		defer conn.CloseNow()
		offered := false
		for {
			var m runnerproto.Message
			if wsjson.Read(ctx, conn, &m) != nil {
				return
			}
			switch {
			case m.Type == "hello":
				hello <- *m.Hello
				pending := runnerproto.Frame("session.pending")
				pending.SessionID = session.ID
				if wsjson.Write(ctx, conn, runnerproto.Frame("hello.ack")) != nil || wsjson.Write(ctx, conn, pending) != nil {
					return
				}
			case m.Type == "claim":
				ack := runnerproto.Frame("claim.ack")
				ack.SessionID, ack.Session = session.ID, &session
				ack.CredentialPath = "/config/claude/01234567890123456789012345678901"
				if wsjson.Write(ctx, conn, ack) != nil {
					return
				}
			case m.Type == "heartbeat":
				if wsjson.Write(ctx, conn, runnerproto.Frame("heartbeat.ack")) != nil {
					return
				}
			case m.Type == "workspace.status" && m.Status == "ready":
				turn := runnerproto.Frame("turn.start")
				turn.SessionID, turn.TurnID, turn.Input = session.ID, session.TurnID, "synthetic"
				turn.Settings = &runnerproto.TurnSettings{Model: session.Model}
				sign(key, session, &turn)
				offered = true
				if wsjson.Write(ctx, conn, turn) != nil {
					return
				}
			case offered && m.SessionID == session.ID:
				select {
				case reported <- m:
				default:
				}
			}
		}
	}))
	defer server.Close()
	exe, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	result := make(chan error, 1)
	go func() {
		result <- Run(ctx, Options{Config: Config{PoolURL: server.URL, RunnerID: session.RunnerID, Token: "pool-runner-" + session.RunnerID + ".synthetic", CommandKey: encoded}, WorkspaceRoot: t.TempDir(), Executables: map[string]string{"claude": exe}, HTTPClient: server.Client()})
	}()
	var event runnerproto.Message
	select {
	case announced := <-hello:
		if announced.CommandKeyID != runnerproto.CommandKeyID(key) {
			t.Fatalf("hello command key id = %q", announced.CommandKeyID)
		}
	case <-ctx.Done():
		t.Fatal("runner sent no hello")
	}
	select {
	case event = <-reported:
	case err := <-result:
		t.Fatalf("runner stopped early: %v", err)
	case <-ctx.Done():
		t.Fatal("runner reported nothing after the turn")
	}
	cancel()
	<-result
	return event
}

func TestDaemonRunsOnlyTurnsSignedByThePairedDevice(t *testing.T) {
	signed := runSignedTurn(t, func(key []byte, s runnerproto.Session, m *runnerproto.Message) {
		m.IssuedAt = time.Now().UnixMilli()
		m.Signatures = map[string]string{runnerproto.CommandKeyID(key): runnerproto.SignCommand(key, runnerproto.TurnSigningInput(s.ID, m.IssuedAt, s.Agent, s.PermissionMode, s.Workspace, m.Input))}
	})
	if signed.Type != "turn.started" {
		t.Fatalf("signed turn reported %s: %s", signed.Type, signed.Content)
	}
	unsigned := runSignedTurn(t, func([]byte, runnerproto.Session, *runnerproto.Message) {})
	if unsigned.Type != "execution.error" || unsigned.TurnID != "turn-one" {
		t.Fatalf("unsigned turn reported %s for %q, want execution.error", unsigned.Type, unsigned.TurnID)
	}
}
