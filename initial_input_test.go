package main

import (
	"encoding/json"
	"strings"
	"testing"

	"codex-pool-proxy/internal/runnerproto"
)

func TestInitialInputSurvivesRejectionRestartAndLostAcknowledgement(t *testing.T) {
	h, b, login, principal := brokerFixture(t)
	r := brokerRunner(t, b, principal)
	q := runnerproto.CreateSession{Agent: "claude", Model: "synthetic-model", PermissionMode: "gated", Workspace: runnerproto.Workspace{Mode: "ephemeral"}, InitialInput: "Keep this first prompt"}
	s, err := b.create(principal, login.ClientID, q)
	if err != nil {
		t.Fatal(err)
	}
	if s.PendingInput == nil || s.PendingInput.Input != q.InitialInput || s.TurnID != "" || b.sessions[s.ID].QueuedInput != "" {
		t.Fatalf("draft was not stored separately from executable input: %+v", s)
	}
	if _, err := b.session("another-principal", s.ID); err == nil {
		t.Fatal("draft leaked to another account")
	}
	if err := b.claim(r, s.ID); err != nil {
		t.Fatal(err)
	}
	brokerEvent(t, b, r, s.ID, "workspace.status", func(m *runnerproto.Message) { m.Status = "ready" })
	input := runnerproto.TurnInput{Input: q.InitialInput, PendingInputID: s.PendingInput.ID}
	if _, err := b.startTurn(principal, s.ID, input, func(*runnerproto.CreateSession) error { return sessionError(503, "temporarily unavailable") }); err == nil {
		t.Fatal("expected transient rejection")
	}
	state, _ := b.session(principal, s.ID)
	if state.PendingInput == nil || state.PendingInput.Input != input.Input {
		t.Fatal("rejection lost draft")
	}
	restored, err := newSessionBroker(h.passport)
	if err != nil {
		t.Fatal(err)
	}
	state, _ = restored.session(principal, s.ID)
	if state.PendingInput == nil || *state.PendingInput != *s.PendingInput {
		t.Fatal("restart lost draft")
	}
	if err := restored.connectRunner(r); err != nil {
		t.Fatal(err)
	}
	if err := restored.claim(r, s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := restored.startTurn(principal, s.ID, runnerproto.TurnInput{Input: "another message"}, authorizeSyntheticSession); err == nil {
		t.Fatal("another message displaced pending draft")
	}
	turn, err := restored.startTurn(principal, s.ID, input, authorizeSyntheticSession)
	if err != nil {
		t.Fatal(err)
	}
	state, _ = restored.session(principal, s.ID)
	if state.PendingInput != nil {
		t.Fatal("acknowledged draft stayed pending")
	}
	cursor := restored.sessions[s.ID].EventCursor
	for i := 0; i < 3; i++ {
		duplicate, err := restored.startTurn(principal, s.ID, input, authorizeSyntheticSession)
		if err != nil || duplicate != turn {
			t.Fatalf("lost-ack retry = %q, %v", duplicate, err)
		}
	}
	if restored.sessions[s.ID].EventCursor != cursor {
		t.Fatal("retry appended duplicate transcript events")
	}
	brokerEvent(t, restored, r, s.ID, "turn.started", nil)
	brokerEvent(t, restored, r, s.ID, "turn.finished", nil)
	restarted, err := newSessionBroker(h.passport)
	if err != nil {
		t.Fatal(err)
	}
	duplicate, err := restarted.startTurn(principal, s.ID, input, authorizeSyntheticSession)
	if err != nil || duplicate != turn {
		t.Fatalf("completed retry after restart = %q, %v", duplicate, err)
	}
	input.Input = "changed content"
	if _, err := restarted.startTurn(principal, s.ID, input, authorizeSyntheticSession); err == nil {
		t.Fatal("receipt accepted changed input")
	}
	input.Input = q.InitialInput
	otherModel := "different-model"
	input.Model = &otherModel
	if _, err := restarted.startTurn(principal, s.ID, input, authorizeSyntheticSession); err == nil {
		t.Fatal("receipt accepted changed model")
	}
}

func TestInitialInputValidationAndUnsignedSubmission(t *testing.T) {
	t.Setenv("POOL_JWT_SECRET", "synthetic-draft-test-secret")
	h, principal, _, _, _, _ := memberQuotaFixture(t)
	saveQuotaPolicy(t, h, principal, ClientPolicy{Providers: PolicySelector{Allow: []string{"*"}}})
	login, err := h.passport.createAppToken(h.passport.principal(principal), getPoolJWTSecret())
	if err != nil {
		t.Fatal(err)
	}
	for _, initial := range []string{" ", strings.Repeat("x", runnerproto.MaxInputBytes+1)} {
		q := runnerproto.CreateSession{Agent: "claude", Model: "synthetic", PermissionMode: "gated", Workspace: runnerproto.Workspace{Mode: "ephemeral"}, InitialInput: initial}
		if validateSessionRequest(q) == nil {
			t.Fatal("invalid draft accepted")
		}
	}
	created := appAuthRequest(h, "POST", "/api/sessions", login.Token, `{"agent":"codex","model":"gpt-5.6-sol","permission_mode":"gated","workspace":{"mode":"ephemeral"},"initial_input":"stored, not authorized"}`)
	if created.Code != 201 {
		t.Fatalf("create: %d %s", created.Code, created.Body.String())
	}
	var s runnerproto.Session
	if err := json.Unmarshal(created.Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	if s.PendingInput == nil {
		t.Fatal("draft missing from creation acknowledgement")
	}
	response := appAuthRequest(h, "POST", "/api/sessions/"+s.ID+"/turns", login.Token, `{"input":"stored, not authorized","pending_input_id":"`+s.PendingInput.ID+`"}`)
	if response.Code != 400 {
		t.Fatal("unsigned draft submission accepted")
	}
	b, err := h.sessionBroker()
	if err != nil {
		t.Fatal(err)
	}
	state, err := b.session(principal, s.ID)
	if err != nil || state.PendingInput == nil || state.TurnID != "" {
		t.Fatal("draft authorized unsigned execution")
	}
}
