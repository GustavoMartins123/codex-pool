package runner

import (
	"context"
	"os"
	"testing"
	"time"

	"codex-pool-proxy/internal/runnerproto"
)

func TestAgentsChangeModelAndEffortInsideTheSameNativeSession(t *testing.T) {
	for _, kind := range []string{"codex", "claude"} {
		t.Run(kind, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			exe, err := os.Executable()
			if err != nil {
				t.Fatal(err)
			}
			events := make(chan runnerproto.Message, 32)
			session := runnerproto.Session{ID: "synthetic-session", Agent: kind, Model: "synthetic-model", PermissionMode: "gated"}
			emit := func(m runnerproto.Message) error { events <- m; return nil }
			var agent agentProcess
			if kind == "codex" {
				agent, err = startCodex(ctx, exe, t.TempDir(), t.TempDir(), "https://pool.example", "synthetic", session, emit, func(int) {})
			} else {
				agent, err = startClaude(ctx, exe, t.TempDir(), t.TempDir(), "https://pool.example", "synthetic", session, emit, func(int) {})
			}
			if err != nil {
				t.Fatal(err)
			}
			defer agent.Close()
			checkpoint := agent.NativeCheckpoint()
			for index, settings := range []runnerproto.TurnSettings{{Model: "synthetic-model", ReasoningEffort: "low"}, {Model: "another-model", ReasoningEffort: "high"}} {
				id := []string{"first", "second"}[index]
				if err := agent.StartTurn(id, "report-settings", settings); err != nil {
					t.Fatal(err)
				}
				text := ""
			loop:
				for {
					select {
					case m := <-events:
						if m.Type == "turn.event" && m.Kind == "text" {
							text += m.Content
						}
						if m.Type == "turn.finished" {
							break loop
						}
					case <-ctx.Done():
						t.Fatal("turn did not finish")
					}
				}
				if text != settings.Model+":"+settings.ReasoningEffort {
					t.Fatalf("agent received %q", text)
				}
				if got := agent.NativeCheckpoint(); got != checkpoint {
					t.Fatalf("native session changed: %+v", got)
				}
			}
			if kind == "claude" {
				if err := agent.StartTurn("reset", "report-settings", runnerproto.TurnSettings{Model: "another-model"}); err != nil {
					t.Fatal(err)
				}
			} else if err := agent.StartTurn("reset", "report-settings", runnerproto.TurnSettings{Model: "another-model"}); err == nil {
				t.Fatal("silently kept a previous Codex effort after an unknown default")
			}
		})
	}
}
