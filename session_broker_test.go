package main

import (
	"bufio"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"codex-pool-proxy/internal/runnerproto"
	"github.com/coder/websocket"
	"github.com/coder/websocket/wsjson"
	"go.etcd.io/bbolt"
)

func brokerFixture(t *testing.T) (*proxyHandler, *sessionBroker, *appLoginResponse, string) {
	t.Helper()
	h := appAuthHandler(t)
	login := appSignIn(t, h)
	b, err := h.sessionBroker()
	if err != nil {
		t.Fatal(err)
	}
	pr := h.passport.byLogin("brute@example.com")
	return h, b, &login, pr.ID
}

func brokerRunner(t *testing.T, b *sessionBroker, principal string) *connectedRunner {
	t.Helper()
	enrollment, err := b.enrollment(principal)
	if err != nil {
		t.Fatal(err)
	}
	credentials, err := b.enroll(runnerproto.EnrollRequest{Nonce: enrollment.Nonce, Name: "synthetic runner"})
	if err != nil {
		t.Fatal(err)
	}
	r := &connectedRunner{id: credentials.RunnerID, principal: principal, hello: runnerproto.Hello{Agents: []string{"codex", "claude"}, OS: "test", Arch: "test", AllowedPaths: []string{"/approved"}}, out: make(chan runnerproto.Message, 128), stop: func() {}, lastSeen: b.now()}
	if err := b.connectRunner(r); err != nil {
		t.Fatal(err)
	}
	return r
}

func brokerSession(t *testing.T, b *sessionBroker, principal, client string) *runnerproto.Session {
	t.Helper()
	s, err := b.create(principal, client, runnerproto.CreateSession{Agent: "claude", Model: "synthetic-model", Workspace: runnerproto.Workspace{Mode: "ephemeral"}, PermissionMode: "gated", Prompt: "synthetic input"})
	if err != nil {
		t.Fatal(err)
	}
	return s
}

func brokerEvent(t *testing.T, b *sessionBroker, r *connectedRunner, id, kind string, configure func(*runnerproto.Message)) {
	t.Helper()
	b.mu.Lock()
	s := copySession(b.sessions[id])
	b.mu.Unlock()
	m := runnerproto.Frame(kind)
	m.SessionID = id
	m.TurnID = s.TurnID
	m.Sequence = s.LastSequence + 1
	if configure != nil {
		configure(&m)
	}
	if err := b.runnerEvent(r, m); err != nil {
		t.Fatalf("%s: %v", kind, err)
	}
}

func TestRunnerEnrollmentIsSingleUseAndRevocable(t *testing.T) {
	_, b, _, principal := brokerFixture(t)
	e, err := b.enrollment(principal)
	if err != nil {
		t.Fatal(err)
	}
	var won atomic.Int32
	var token runnerproto.EnrollResponse
	var mu sync.Mutex
	var wg sync.WaitGroup
	for range 12 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			result, err := b.enroll(runnerproto.EnrollRequest{Nonce: e.Nonce, Name: "test"})
			if err == nil {
				won.Add(1)
				mu.Lock()
				token = *result
				mu.Unlock()
			}
		}()
	}
	wg.Wait()
	if won.Load() != 1 {
		t.Fatalf("successful enrollments=%d", won.Load())
	}
	r := httptest.NewRequest("GET", "/api/runner/ws", nil)
	r.Header.Set("Authorization", "Bearer "+token.Token)
	if _, err := b.runnerCredential(r); err != nil {
		t.Fatal(err)
	}
	_ = b.store.db.View(func(tx *bbolt.Tx) error {
		if strings.Contains(string(tx.Bucket([]byte(bucketRunners)).Get([]byte(token.RunnerID))), token.Token) {
			t.Fatal("runner token persisted in plaintext")
		}
		return nil
	})
	if err := b.revokeRunner(principal, token.RunnerID); err != nil {
		t.Fatal(err)
	}
	if _, err := b.runnerCredential(r); err == nil {
		t.Fatal("revoked credential accepted")
	}
	e, err = b.enrollment(principal)
	if err != nil {
		t.Fatal(err)
	}
	clock := b.now().Add(11 * time.Minute)
	b.now = func() time.Time { return clock }
	if _, err := b.enroll(runnerproto.EnrollRequest{Nonce: e.Nonce, Name: "test"}); err == nil {
		t.Fatal("expired nonce accepted")
	}
}

func TestRunnerCredentialRotationRequiresIdleRunnerAndInvalidatesOldToken(t *testing.T) {
	_, b, login, principal := brokerFixture(t)
	e, err := b.enrollment(principal)
	if err != nil {
		t.Fatal(err)
	}
	old, err := b.enroll(runnerproto.EnrollRequest{Nonce: e.Nonce, Name: "runner"})
	if err != nil {
		t.Fatal(err)
	}
	rotation, err := b.newEnrollment(principal, old.RunnerID)
	if err != nil {
		t.Fatal(err)
	}
	updated, err := b.enroll(runnerproto.EnrollRequest{Nonce: rotation.Nonce, Name: "renamed"})
	if err != nil {
		t.Fatal(err)
	}
	if updated.RunnerID != old.RunnerID || updated.Token == old.Token {
		t.Fatal("rotation did not preserve identity and change secret")
	}
	r := httptest.NewRequest("GET", "/api/runner/ws", nil)
	r.Header.Set("Authorization", "Bearer "+old.Token)
	if _, err := b.runnerCredential(r); err == nil {
		t.Fatal("old runner token accepted")
	}
	r.Header.Set("Authorization", "Bearer "+updated.Token)
	if _, err := b.runnerCredential(r); err != nil {
		t.Fatal(err)
	}
	active := &connectedRunner{id: old.RunnerID, principal: principal, hello: runnerproto.Hello{Agents: []string{"claude"}, OS: "test", Arch: "test"}, out: make(chan runnerproto.Message, 128), stop: func() {}}
	if err := b.connectRunner(active); err != nil {
		t.Fatal(err)
	}
	s := brokerSession(t, b, principal, login.ClientID)
	if err := b.claim(active, s.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := b.newEnrollment(principal, old.RunnerID); err == nil {
		t.Fatal("rotation allowed with an active session")
	}
	if _, err := b.newEnrollment("different-principal", old.RunnerID); err == nil {
		t.Fatal("cross principal rotation allowed")
	}
}

func TestClaimReleaseAndCancelledApprovalRemainConsistent(t *testing.T) {
	_, b, login, principal := brokerFixture(t)
	r := brokerRunner(t, b, principal)
	s := brokerSession(t, b, principal, login.ClientID)
	if err := b.claim(r, s.ID); err != nil {
		t.Fatal(err)
	}
	client := b.sessions[s.ID].LLMClientID
	if err := b.releaseClaim(r, s.ID); err != nil {
		t.Fatal(err)
	}
	if _, _, ok := b.store.credentialState(principal + "-c-" + client); ok {
		t.Fatal("released claim credential stayed valid")
	}
	if err := b.claim(r, s.ID); err != nil {
		t.Fatal(err)
	}
	brokerEvent(t, b, r, s.ID, "workspace.status", func(m *runnerproto.Message) { m.Status = "ready" })
	brokerEvent(t, b, r, s.ID, "turn.started", nil)
	brokerEvent(t, b, r, s.ID, "approval.request", func(m *runnerproto.Message) { m.ApprovalID = "cancelled-request"; m.Command = "synthetic" })
	brokerEvent(t, b, r, s.ID, "approval.cancelled", func(m *runnerproto.Message) { m.ApprovalID = "cancelled-request" })
	state, _ := b.session(principal, s.ID)
	if len(state.Approvals) != 0 || state.State != "running" {
		t.Fatal("cancelled approval stayed pending")
	}
	if err := b.resolveApproval(principal, s.ID, "cancelled-request", "approve"); err == nil {
		t.Fatal("cancelled approval was approved")
	}
	if err := b.releaseClaim(r, s.ID); err == nil {
		t.Fatal("running claim released")
	}
}

func TestSessionLeaseReattachApprovalTimeoutAndCancellation(t *testing.T) {
	_, b, login, principal := brokerFixture(t)
	clock := b.now()
	b.now = func() time.Time { return clock }
	r := brokerRunner(t, b, principal)
	other := brokerRunner(t, b, principal)
	s := brokerSession(t, b, principal, login.ClientID)
	if err := b.claim(r, s.ID); err != nil {
		t.Fatal(err)
	}
	if err := b.claim(other, s.ID); err == nil {
		t.Fatal("second runner won an existing claim")
	}
	foreign := *r
	foreign.principal = "another-principal"
	if err := b.claim(&foreign, s.ID); err == nil {
		t.Fatal("cross principal claim accepted")
	}
	brokerEvent(t, b, r, s.ID, "workspace.status", func(m *runnerproto.Message) { m.Status = "ready" })
	brokerEvent(t, b, r, s.ID, "turn.started", nil)
	brokerEvent(t, b, r, s.ID, "approval.request", func(m *runnerproto.Message) {
		m.ApprovalID = "approval-1"
		m.Command = "synthetic command"
		m.Context = "test"
	})
	clock = clock.Add(11 * time.Minute)
	if err := b.sweep(); err != nil {
		t.Fatal(err)
	}
	state, err := b.session(principal, s.ID)
	if err != nil {
		t.Fatal(err)
	}
	if state.State != "orphaned" || len(state.Approvals) != 0 {
		t.Fatalf("state=%s pending=%d", state.State, len(state.Approvals))
	}
	if err := b.claim(other, s.ID); err == nil {
		t.Fatal("cross runner reattach accepted")
	}
	if err := b.claim(r, s.ID); err != nil {
		t.Fatal(err)
	}
	if err := b.cancelTurn(principal, s.ID); err != nil {
		t.Fatal(err)
	}
	brokerEvent(t, b, r, s.ID, "turn.finished", func(m *runnerproto.Message) { m.ExitCode = 130 })
	state, _ = b.session(principal, s.ID)
	if state.State != "waiting_input" || state.Turns != 1 {
		t.Fatal("turn did not complete")
	}
	if _, err := b.startTurn(principal, s.ID, "next input"); err != nil {
		t.Fatal(err)
	}
	if err := b.requestClose(principal, s.ID); err != nil {
		t.Fatal(err)
	}
	brokerEvent(t, b, r, s.ID, "process.exit", nil)
	state, _ = b.session(principal, s.ID)
	if state.State != "closed" || state.ClosedReason != "cancelled" {
		t.Fatalf("close=%s/%s", state.State, state.ClosedReason)
	}
	if _, _, ok := b.store.credentialState(principal + "-c-" + b.sessions[s.ID].LLMClientID); ok {
		t.Fatal("closed session LLM credential remained valid")
	}
}

func TestBrokerRestoresLeaseAsOrphanedAndDeduplicatesEvents(t *testing.T) {
	h, b, login, principal := brokerFixture(t)
	r := brokerRunner(t, b, principal)
	s := brokerSession(t, b, principal, login.ClientID)
	if err := b.claim(r, s.ID); err != nil {
		t.Fatal(err)
	}
	brokerEvent(t, b, r, s.ID, "workspace.status", func(m *runnerproto.Message) { m.Status = "ready" })
	brokerEvent(t, b, r, s.ID, "turn.started", nil)
	m := runnerproto.Frame("turn.event")
	m.SessionID = s.ID
	m.TurnID = s.TurnID
	m.Sequence = 3
	m.Kind = "text"
	m.Content = "synthetic delta"
	if err := b.runnerEvent(r, m); err != nil {
		t.Fatal(err)
	}
	if err := b.runnerEvent(r, m); err != nil {
		t.Fatal(err)
	}
	count := 0
	for _, e := range b.streams[s.ID].history {
		if e.Type == "turn.delta" {
			count++
		}
	}
	if count != 1 {
		t.Fatalf("delta applied %d times", count)
	}
	restored, err := newSessionBroker(h.passport)
	if err != nil {
		t.Fatal(err)
	}
	state, _ := restored.session(principal, s.ID)
	if state.State != "orphaned" {
		t.Fatal(state.State)
	}
	r.out = make(chan runnerproto.Message, 128)
	if err := restored.connectRunner(r); err != nil {
		t.Fatal(err)
	}
	if err := restored.claim(r, s.ID); err != nil {
		t.Fatal(err)
	}
	if restored.sessions[s.ID].LastSequence != 3 {
		t.Fatal("sequence not restored")
	}
	m.Sequence = 5
	if err := restored.runnerEvent(r, m); err == nil {
		t.Fatal("sequence gap accepted")
	}
}

func TestSessionTimeoutAndAppRevocation(t *testing.T) {
	h, b, login, principal := brokerFixture(t)
	clock := b.now()
	b.now = func() time.Time { return clock }
	s := brokerSession(t, b, principal, login.ClientID)
	clock = clock.Add(sessionPendingTTL + time.Second)
	if err := b.sweep(); err != nil {
		t.Fatal(err)
	}
	state, _ := b.session(principal, s.ID)
	if state.ClosedReason != "no_runner" {
		t.Fatal(state.ClosedReason)
	}
	s = brokerSession(t, b, principal, login.ClientID)
	if err := h.passport.revokeClient(principal, principal, login.ClientID); err != nil {
		t.Fatal(err)
	}
	if err := b.sweep(); err != nil {
		t.Fatal(err)
	}
	state, _ = b.session(principal, s.ID)
	if state.ClosedReason != "app_credential_revoked" {
		t.Fatal(state.ClosedReason)
	}
}

func TestPasswordChangeInvalidatesSessionEvenIfCreationWasDelayed(t *testing.T) {
	h, b, login, principal := brokerFixture(t)
	if _, err := h.passport.ChangePrincipalPassword(principal, principal, "another correct horse battery"); err != nil {
		t.Fatal(err)
	}
	clock := time.Now().Add(3 * time.Second)
	b.now = func() time.Time { return clock }
	s := brokerSession(t, b, principal, login.ClientID)
	r := brokerRunner(t, b, principal)
	if err := b.claim(r, s.ID); err == nil {
		t.Fatal("claim accepted a session authenticated before password change")
	}
	if err := b.sweep(); err != nil {
		t.Fatal(err)
	}
	state, _ := b.session(principal, s.ID)
	if state.ClosedReason != "app_credential_revoked" {
		t.Fatal("invalidated app retained its session")
	}
}

func TestSessionHTTPAndRunnerWebSocketRelay(t *testing.T) {
	t.Setenv("POOL_JWT_SECRET", "synthetic-session-test-secret")
	h, principal, _, _, _, _ := memberQuotaFixture(t)
	saveQuotaPolicy(t, h, principal, ClientPolicy{Providers: PolicySelector{Allow: []string{"*"}}})
	login, err := h.passport.createAppToken(h.passport.principal(principal), getPoolJWTSecret())
	if err != nil {
		t.Fatal(err)
	}
	create := appAuthRequest(h, "POST", "/api/sessions", login.Token, `{"agent":"codex","model":"gpt-5.6-sol","workspace":{"mode":"ephemeral"},"permission_mode":"gated","prompt":"synthetic task"}`)
	if create.Code != 201 {
		t.Fatalf("create=%d %s", create.Code, create.Body.String())
	}
	if create.Header().Get("Content-Type") != "application/json" {
		t.Fatal("session creation response lacks JSON content type")
	}
	var session runnerproto.Session
	if err := json.Unmarshal(create.Body.Bytes(), &session); err != nil {
		t.Fatal(err)
	}
	for _, body := range []string{`{"agent":"codex","model":"gpt-5.6-sol","workspace":{"mode":"path","target":"/unsafe"},"permission_mode":"auto"}`, `{"agent":"codex","model":"not-in-catalog","workspace":{"mode":"ephemeral"},"permission_mode":"gated"}`} {
		if response := appAuthRequest(h, "POST", "/api/sessions", login.Token, body); response.Code < 400 {
			t.Fatal("invalid request accepted")
		}
	}
	server := httptest.NewServer(h)
	defer server.Close()
	b, _ := h.sessionBroker()
	nonce, err := b.enrollment(principal)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := b.enroll(runnerproto.EnrollRequest{Nonce: nonce.Nonce, Name: "test"})
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	conn, _, err := websocket.Dial(ctx, "ws"+strings.TrimPrefix(server.URL, "http")+"/api/runner/ws", &websocket.DialOptions{HTTPHeader: http.Header{"Authorization": []string{"Bearer " + credential.Token}}})
	if err != nil {
		t.Fatal(err)
	}
	defer conn.CloseNow()
	hello := runnerproto.Frame("hello")
	hello.Hello = &runnerproto.Hello{Agents: []string{"codex"}, OS: "test", Arch: "test"}
	if err := wsjson.Write(ctx, conn, hello); err != nil {
		t.Fatal(err)
	}
	read := func(kind string) runnerproto.Message {
		t.Helper()
		for {
			var m runnerproto.Message
			if err := wsjson.Read(ctx, conn, &m); err != nil {
				t.Fatal(err)
			}
			if m.Type == kind {
				return m
			}
		}
	}
	read("hello.ack")
	read("session.pending")
	claim := runnerproto.Frame("claim")
	claim.SessionID = session.ID
	_ = wsjson.Write(ctx, conn, claim)
	ack := read("claim.ack")
	if !strings.HasPrefix(ack.CredentialPath, "/config/codex/") {
		t.Fatal("claim nonce missing")
	}
	config := appAuthRequest(h, "GET", ack.CredentialPath, "", "")
	if config.Code != 200 {
		t.Fatalf("credential download=%d", config.Code)
	}
	if repeat := appAuthRequest(h, "GET", ack.CredentialPath, "", ""); repeat.Code != 404 {
		t.Fatal("credential nonce reused")
	}
	request, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/api/sessions/"+session.ID+"/events", nil)
	request.Header.Set("Authorization", "Bearer "+login.Token)
	response, err := http.DefaultClient.Do(request)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	reader := bufio.NewReader(response.Body)
	line, err := reader.ReadString('\n')
	if err != nil || !strings.Contains(line, "event: state") {
		t.Fatalf("SSE initial state=%q %v", line, err)
	}
	ready := runnerproto.Frame("workspace.status")
	ready.SessionID = session.ID
	ready.Sequence = 1
	ready.Status = "ready"
	_ = wsjson.Write(ctx, conn, ready)
	read("event.ack")
	turn := read("turn.start")
	if turn.Input != "synthetic task" || turn.TurnID == "" {
		t.Fatal("initial turn not relayed")
	}
	if err := b.revokeRunner(principal, credential.RunnerID); err != nil {
		t.Fatal(err)
	}
	if state, _ := b.session(principal, session.ID); state.State != "closed" {
		t.Fatal("runner revocation did not close session")
	}
}
