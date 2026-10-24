package main

import (
	"errors"
	"testing"

	"codex-pool-proxy/internal/runnerproto"
)

func TestSessionTurnSettingsAreAuthorizedAndSavedAtomically(t *testing.T) {
	_, b, login, principal := brokerFixture(t)
	r := brokerRunner(t, b, principal)
	s := brokerSession(t, b, principal, login.ClientID)
	if err := b.claim(r, s.ID); err != nil {
		t.Fatal(err)
	}
	brokerEvent(t, b, r, s.ID, "workspace.status", func(m *runnerproto.Message) { m.Status = "ready" })
	brokerEvent(t, b, r, s.ID, "turn.started", nil)
	brokerEvent(t, b, r, s.ID, "turn.finished", nil)
	for len(r.out) > 0 {
		<-r.out
	}
	model, effort := "another-model", "high"
	request := runnerproto.TurnInput{Input: "continue", Model: &model, ReasoningEffort: &effort}
	denied := errors.New("model denied")
	before := copySession(b.sessions[s.ID])
	if _, err := b.startTurn(principal, s.ID, request, nil); err == nil {
		t.Fatal("missing authorizer accepted")
	}
	if _, err := b.startTurn(principal, s.ID, request, func(*runnerproto.CreateSession) error { return denied }); !errors.Is(err, denied) {
		t.Fatal(err)
	}
	after, _ := b.session(principal, s.ID)
	if after.Model != before.Model || after.ReasoningEffort != before.ReasoningEffort || after.State != "waiting_input" || len(r.out) != 0 || b.sessions[s.ID].EventCursor != before.EventCursor {
		t.Fatal("rejected settings changed the session")
	}
	turn, err := b.startTurn(principal, s.ID, request, func(q *runnerproto.CreateSession) error {
		if q.Agent != "claude" || q.Model != model || q.ReasoningEffort != effort {
			t.Fatalf("authorization saw wrong settings: %+v", q)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	frame := <-r.out
	if frame.Type != "turn.start" || frame.Settings == nil || frame.Settings.Model != model || frame.Settings.ReasoningEffort != effort || frame.TurnID != turn {
		t.Fatalf("frame=%+v", frame)
	}
	after, _ = b.session(principal, s.ID)
	if after.Model != model || after.ReasoningEffort != effort || after.ConversationID != before.ConversationID || after.RunnerID != before.RunnerID {
		t.Fatalf("session=%+v", after)
	}
	if _, err := b.startTurn(principal, s.ID, request, nil); err == nil {
		t.Fatal("changed settings in a running turn")
	}
	if _, err := b.startTurn("another-principal", s.ID, request, nil); err == nil {
		t.Fatal("foreign principal changed session settings")
	}
}

func TestSessionReasoningPolicyRejectsUnsupportedValuesInsteadOfDowngrading(t *testing.T) {
	for _, tc := range []struct {
		name, agent, effort, limit, defaultEffort, want string
		levels                                          []string
		reject                                          bool
	}{
		{name: "explicit", agent: "codex", effort: "high", levels: []string{"low", "high"}, want: "high"},
		{name: "catalog default", agent: "codex", defaultEffort: "low", levels: []string{"low", "high"}, want: "low"},
		{name: "provider default", agent: "claude", want: ""},
		{name: "unsupported model effort", agent: "claude", effort: "xhigh", levels: []string{"low", "high", "max"}, reject: true},
		{name: "unsupported agent effort", agent: "claude", effort: "minimal", levels: []string{"minimal", "high"}, reject: true},
		{name: "unknown capability", agent: "codex", effort: "high", reject: true},
		{name: "account cap", agent: "codex", effort: "high", levels: []string{"low", "high"}, limit: "low", reject: true},
		{name: "unknown value", agent: "codex", effort: "super", levels: []string{"super"}, reject: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := sessionReasoningEffort(tc.agent, tc.effort, poolModelDescriptor{ReasoningEfforts: tc.levels, DefaultReasoningEffort: tc.defaultEffort}, tc.limit)
			if (err != nil) != tc.reject || (!tc.reject && got != tc.want) {
				t.Fatalf("effort=%q err=%v", got, err)
			}
		})
	}
}
