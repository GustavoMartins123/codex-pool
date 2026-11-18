package main

import (
	"bufio"
	"cmp"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"codex-pool-proxy/internal/runnerproto"
	"go.etcd.io/bbolt"
)

func consoleFixture(t *testing.T) (*proxyHandler, *sessionBroker, string, string, string, *appLoginResponse) {
	t.Helper()
	t.Setenv("POOL_JWT_SECRET", "console-test-jwt-secret")
	h, principal, session, csrf, _, _ := memberQuotaFixture(t)
	saveQuotaPolicy(t, h, principal, ClientPolicy{Providers: PolicySelector{Allow: []string{"*"}}})
	login, err := h.passport.createAppToken(h.passport.principal(principal), getPoolJWTSecret())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Second)
	t.Cleanup(cancel)
	if err := h.startSessionBroker(ctx); err != nil {
		t.Fatal(err)
	}
	b, err := h.sessionBroker()
	if err != nil {
		t.Fatal(err)
	}
	return h, b, principal, session, csrf, login
}

func consoleRequest(h *proxyHandler, method, path, session, csrf string, body []byte) *httptest.ResponseRecorder {
	w := httptest.NewRecorder()
	h.ServeHTTP(w, passportContributionRequest(method, path, session, csrf, body))
	return w
}

func consoleCreateSession(t *testing.T, h *proxyHandler, token string) runnerproto.Session {
	t.Helper()
	created := appAuthRequest(h, "POST", "/api/sessions", token, `{"agent":"codex","model":"gpt-5.6-sol","workspace":{"mode":"ephemeral"},"permission_mode":"gated"}`)
	if created.Code != 201 {
		t.Fatalf("create=%d %s", created.Code, created.Body.String())
	}
	var s runnerproto.Session
	if err := json.Unmarshal(created.Body.Bytes(), &s); err != nil {
		t.Fatal(err)
	}
	return s
}

func consoleAuditActions(t *testing.T, h *proxyHandler) map[string]string {
	t.Helper()
	actions := map[string]string{}
	if err := h.passport.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket([]byte(bucketPassportAudit)).ForEach(func(_, raw []byte) error {
			var e AuditEntry
			if json.Unmarshal(raw, &e) == nil {
				actions[e.Action] = e.Detail
			}
			return nil
		})
	}); err != nil {
		t.Fatal(err)
	}
	return actions
}

func TestConsoleCookieLifecycleAndAuditParity(t *testing.T) {
	h, b, principal, session, csrf, login := consoleFixture(t)
	s := consoleCreateSession(t, h, login.Token)
	if w := consoleRequest(h, "GET", "/api/sessions/"+s.ID, session, "", nil); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	list := consoleRequest(h, "GET", "/api/conversations?storage=active&limit=10", session, "", nil)
	if list.Code != 200 || !strings.Contains(list.Body.String(), s.ConversationID) {
		t.Fatal(list.Code, list.Body.String())
	}
	if w := consoleRequest(h, "GET", "/api/conversations/"+s.ConversationID, session, "", nil); w.Code != 200 {
		t.Fatal(w.Code, w.Body.String())
	}
	history := consoleRequest(h, "GET", "/api/conversations/"+s.ConversationID+"/history?limit=10", session, "", nil)
	if history.Code != 200 {
		t.Fatal(history.Code, history.Body.String())
	}
	var page conversationHistoryPage
	if err := json.Unmarshal(history.Body.Bytes(), &page); err != nil || len(page.Events) == 0 {
		t.Fatal("history page empty", err)
	}
	if w := consoleRequest(h, "GET", "/api/conversations/"+s.ConversationID+"/history", session, "", nil); w.Code != 400 {
		t.Fatal("history without limit:", w.Code)
	}
	if w := consoleRequest(h, "GET", "/api/conversations", session, "", nil); w.Code != 400 {
		t.Fatal("list without limit:", w.Code)
	}
	if w := consoleRequest(h, "DELETE", "/api/sessions/"+s.ID, session, csrf, nil); w.Code != 204 {
		t.Fatal(w.Code, w.Body.String())
	}
	closed := consoleRequest(h, "GET", "/api/sessions/"+s.ID, session, "", nil)
	if closed.Code != 200 || !strings.Contains(closed.Body.String(), `"closed"`) {
		t.Fatal(closed.Code, closed.Body.String())
	}
	deleted := consoleRequest(h, "DELETE", "/api/conversations/"+s.ConversationID, session, csrf, nil)
	if deleted.Code != 202 || deleted.Body.Len() != 0 {
		t.Fatalf("delete=%d body=%q", deleted.Code, deleted.Body.String())
	}
	if w := consoleRequest(h, "GET", "/api/conversations/"+s.ConversationID, session, "", nil); w.Code != 404 {
		t.Fatal("deleted conversation still readable:", w.Code)
	}

	live := brokerSession(t, b, principal, login.ClientID)
	r := brokerRunner(t, b, principal)
	if err := b.claim(r, live.ID); err != nil {
		t.Fatal(err)
	}
	brokerEvent(t, b, r, live.ID, "workspace.status", func(m *runnerproto.Message) { m.Status = "ready" })
	brokerEvent(t, b, r, live.ID, "turn.started", nil)
	brokerEvent(t, b, r, live.ID, "approval.request", func(m *runnerproto.Message) { m.ApprovalID = "console-approval"; m.Command = "synthetic" })
	if w := consoleRequest(h, "POST", "/api/sessions/"+live.ID+"/approvals/console-approval", session, csrf, []byte(`{"decision":"deny"}`)); w.Code != 204 {
		t.Fatal("deny:", w.Code, w.Body.String())
	}
	if w := consoleRequest(h, "POST", "/api/sessions/"+live.ID+"/turns:cancel", session, csrf, nil); w.Code != 204 {
		t.Fatal("cancel:", w.Code, w.Body.String())
	}
	runners := consoleRequest(h, "GET", "/api/me/runners", session, "", nil)
	if runners.Code != 200 || !strings.Contains(runners.Body.String(), r.id) {
		t.Fatal(runners.Code, runners.Body.String())
	}
	if w := consoleRequest(h, "DELETE", "/api/me/runners/"+r.id, session, csrf, nil); w.Code != 204 {
		t.Fatal("revoke:", w.Code, w.Body.String())
	}
	denied := startTestPairing(t, b, nil)
	if w := consoleRequest(h, "POST", "/api/me/runner-pairings/"+denied.ID, session, csrf, []byte(`{"decision":"deny"}`)); w.Code != 204 {
		t.Fatal("pairing deny:", w.Code, w.Body.String())
	}
	approved := startTestPairing(t, b, nil)
	if w := consoleRequest(h, "GET", "/api/me/runner-pairings/"+approved.ID, session, "", nil); w.Code != 200 {
		t.Fatal("pairing review:", w.Code, w.Body.String())
	}
	decision, _ := json.Marshal(runnerproto.PairingDecision{Decision: "approve", UserCode: approved.UserCode})
	if w := consoleRequest(h, "POST", "/api/me/runner-pairings/"+approved.ID, session, csrf, decision); w.Code != 204 {
		t.Fatal("pairing approve:", w.Code, w.Body.String())
	}
	actions := consoleAuditActions(t, h)
	for _, action := range []string{"session.created", "session.closed", "conversation.deleted", "session.turn_cancelled", "session.approval", "runner.revoked", "runner.pairing.deny", "runner.pairing.approve"} {
		if _, ok := actions[action]; !ok {
			t.Fatalf("cookie action did not audit %q", action)
		}
	}
	if actions["session.approval"] != "deny" {
		t.Fatal("approval audit lost the decision")
	}
}

func TestConsoleCookieMutationsRequireCSRF(t *testing.T) {
	h, b, _, session, csrf, login := consoleFixture(t)
	s := consoleCreateSession(t, h, login.Token)
	pairing := startTestPairing(t, b, nil)
	mutations := []struct {
		method, path string
		body         []byte
	}{
		{"POST", "/api/sessions", []byte(`{"agent":"codex","model":"gpt-5.6-sol","workspace":{"mode":"ephemeral"},"permission_mode":"gated"}`)},
		{"POST", "/api/sessions/" + s.ID + "/turns", []byte(`{"input":"synthetic"}`)},
		{"POST", "/api/sessions/" + s.ID + "/turns:cancel", nil},
		{"POST", "/api/sessions/" + s.ID + "/approvals/any", []byte(`{"decision":"deny"}`)},
		{"DELETE", "/api/sessions/" + s.ID, nil},
		{"DELETE", "/api/conversations/" + s.ConversationID, nil},
		{"POST", "/api/conversations/" + s.ConversationID + "/resume", nil},
		{"POST", "/api/me/runner-pairings/" + pairing.ID, []byte(`{"decision":"deny"}`)},
		{"DELETE", "/api/me/runners/any-runner", nil},
	}
	for _, m := range mutations {
		if w := consoleRequest(h, m.method, m.path, session, "", m.body); w.Code != 403 {
			t.Fatalf("%s %s without csrf = %d", m.method, m.path, w.Code)
		}
		r := passportContributionRequest(m.method, m.path, session, csrf, m.body)
		r.Header.Set("X-CSRF-Token", "wrong-"+csrf)
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 403 {
			t.Fatalf("%s %s with wrong header = %d", m.method, m.path, w.Code)
		}
	}
	for _, path := range []string{
		"/api/me/runners",
		"/api/me/runner-pairings",
		"/api/conversations?storage=active&limit=10",
		"/api/sessions/" + s.ID,
		"/api/conversations/" + s.ConversationID,
		"/api/conversations/" + s.ConversationID + "/history?limit=10",
		"/api/me/runner-pairings/" + pairing.ID,
	} {
		if w := consoleRequest(h, "GET", path, session, "", nil); w.Code != 200 {
			t.Fatalf("GET %s without csrf = %d: %s", path, w.Code, w.Body.String())
		}
	}
}

func TestConsoleSignedActionsStayAppOnly(t *testing.T) {
	h, _, _, session, csrf, login := consoleFixture(t)
	s := consoleCreateSession(t, h, login.Token)
	key := []byte("console-guard-command-key-32-bytes")
	keyID := runnerproto.CommandKeyID(key)
	issuedAt := time.Now().UnixMilli()
	signature := runnerproto.SignCommand(key, runnerproto.TurnSigningInput(s.ID, issuedAt, s.Agent, s.PermissionMode, s.Workspace, "synthetic input"))
	signed, _ := json.Marshal(runnerproto.TurnInput{Input: "synthetic input", IssuedAt: issuedAt, Signatures: map[string]string{keyID: signature}})
	for _, tc := range []struct {
		path    string
		body    []byte
		message string
	}{
		{"/api/sessions", []byte(`{"agent":"codex","model":"gpt-5.6-sol","workspace":{"mode":"ephemeral"},"permission_mode":"gated"}`), "creating sessions requires the mobile app"},
		{"/api/sessions/" + s.ID + "/turns", signed, "sending turns requires the mobile app"},
		{"/api/conversations/" + s.ConversationID + "/resume", nil, "resuming conversations requires the mobile app"},
	} {
		w := consoleRequest(h, "POST", tc.path, session, csrf, tc.body)
		if w.Code != 403 || !strings.Contains(w.Body.String(), tc.message) {
			t.Fatalf("%s = %d %s", tc.path, w.Code, w.Body.String())
		}
	}
	if w := consoleRequest(h, "POST", "/api/sessions/"+s.ID+"/turns", session, csrf, []byte(`{"input":"synthetic input"}`)); w.Code != 400 {
		t.Fatal("unsigned turn did not fail the signature gate first:", w.Code)
	}
	if w := appAuthRequest(h, "POST", "/api/sessions/"+s.ID+"/approvals/any", login.Token, `{"decision":"approve"}`); w.Code != 400 {
		t.Fatal("unsigned app approve accepted:", w.Code)
	}
}

func TestConsoleCrossPrincipalIsolation(t *testing.T) {
	h, b, principal, _, _, login := consoleFixture(t)
	other, _, _, _, err := h.passport.createGuest("test", "other-member", "Other", nil)
	if err != nil {
		t.Fatal(err)
	}
	otherSession, otherCSRF, err := h.passport.createSession(other.ID)
	if err != nil {
		t.Fatal(err)
	}
	s := consoleCreateSession(t, h, login.Token)
	for _, tc := range []struct{ method, path string }{
		{"GET", "/api/sessions/" + s.ID},
		{"GET", "/api/conversations/" + s.ConversationID},
		{"DELETE", "/api/conversations/" + s.ConversationID},
		{"GET", "/api/conversations/" + s.ConversationID + "/history?limit=10"},
	} {
		if w := consoleRequest(h, tc.method, tc.path, otherSession, otherCSRF, nil); w.Code != 404 {
			t.Fatalf("%s %s cross principal = %d", tc.method, tc.path, w.Code)
		}
	}
	credential := pairedRunner(t, b, principal)
	if w := consoleRequest(h, "DELETE", "/api/me/runners/"+credential.RunnerID, otherSession, otherCSRF, nil); w.Code != 404 {
		t.Fatal("cross principal revoke:", w.Code)
	}
	var record runnerRecord
	if err := h.passport.db.View(func(tx *bbolt.Tx) error {
		return json.Unmarshal(tx.Bucket([]byte(bucketRunners)).Get([]byte(credential.RunnerID)), &record)
	}); err != nil {
		t.Fatal(err)
	}
	rotation := startTestPairing(t, b, &record)
	if w := consoleRequest(h, "GET", "/api/me/runner-pairings/"+rotation.ID, otherSession, "", nil); w.Code != 409 {
		t.Fatal("cross principal rotation review:", w.Code)
	}
	if w := consoleRequest(h, "POST", "/api/me/runner-pairings/"+rotation.ID, otherSession, otherCSRF, []byte(`{"decision":"deny"}`)); w.Code != 409 {
		t.Fatal("cross principal rotation decision:", w.Code)
	}
	list := consoleRequest(h, "GET", "/api/me/runner-pairings", otherSession, "", nil)
	if list.Code != 200 || strings.Contains(list.Body.String(), rotation.ID) {
		t.Fatal("claimed pairing leaked to another principal:", list.Body.String())
	}
}

func TestConsoleSessionEventsCookieReauth(t *testing.T) {
	h, b, principal, _, _, login := consoleFixture(t)
	other, _, _, _, err := h.passport.createGuest("test", "sse-other", "Other", nil)
	if err != nil {
		t.Fatal(err)
	}
	h.eventHeartbeat = 10 * time.Millisecond
	server := httptest.NewServer(h)
	defer server.Close()
	for _, tc := range []struct {
		name   string
		mutate func(token string)
	}{
		{"expired session", func(token string) {
			digest := hashToken(token)
			if err := h.passport.db.Update(func(tx *bbolt.Tx) error {
				return tx.Bucket([]byte(bucketPassportSessions)).Delete(digest[:])
			}); err != nil {
				t.Fatal(err)
			}
		}},
		{"principal switched", func(token string) {
			digest := hashToken(token)
			if err := h.passport.db.Update(func(tx *bbolt.Tx) error {
				return putJSON(tx.Bucket([]byte(bucketPassportSessions)), string(digest[:]), passportSession{PrincipalID: other.ID, ExpiresAt: time.Now().Add(time.Hour)})
			}); err != nil {
				t.Fatal(err)
			}
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			token, _, err := h.passport.createSession(principal)
			if err != nil {
				t.Fatal(err)
			}
			s := brokerSession(t, b, principal, login.ClientID)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			request, _ := http.NewRequestWithContext(ctx, "GET", server.URL+"/api/conversations/"+s.ConversationID+"/events", nil)
			request.AddCookie(&http.Cookie{Name: "pool_session", Value: token})
			response, err := server.Client().Do(request)
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			if response.StatusCode != 200 {
				t.Fatal(response.StatusCode)
			}
			scanner := bufio.NewScanner(response.Body)
			awaitLine := func(want string) {
				t.Helper()
				for scanner.Scan() {
					if scanner.Text() == want {
						return
					}
				}
				t.Fatalf("stream ended before %q: %v", want, scanner.Err())
			}
			awaitLine("event: state")
			tc.mutate(token)
			awaitLine("event: error")
			if !scanner.Scan() || !strings.Contains(scanner.Text(), "authentication expired") {
				t.Fatalf("expected authentication expired error, got %q", scanner.Text())
			}
		})
	}
}

func TestConsolePendingPairingsVisibility(t *testing.T) {
	h, b, principal, session, csrf, _ := consoleFixture(t)
	other, _, _, _, err := h.passport.createGuest("test", "pairing-other", "Other", nil)
	if err != nil {
		t.Fatal(err)
	}
	otherSession, _, err := h.passport.createSession(other.ID)
	if err != nil {
		t.Fatal(err)
	}
	if w := appAuthRequest(h, "GET", "/api/me/runner-pairings", "", ""); w.Code != 401 {
		t.Fatal("unauthenticated list:", w.Code)
	}
	post := consoleRequest(h, "POST", "/api/me/runner-pairings", session, csrf, nil)
	if post.Code != 405 || post.Header().Get("Allow") != "GET" {
		t.Fatal("POST list:", post.Code, post.Header().Get("Allow"))
	}
	unclaimed := startTestPairing(t, b, nil)
	memberList := consoleRequest(h, "GET", "/api/me/runner-pairings", session, "", nil)
	if memberList.Code != 200 || !strings.Contains(memberList.Body.String(), unclaimed.ID) {
		t.Fatal("unclaimed pairing not discoverable:", memberList.Code, memberList.Body.String())
	}
	if w := consoleRequest(h, "GET", "/api/me/runner-pairings", otherSession, "", nil); w.Code != 200 || !strings.Contains(w.Body.String(), unclaimed.ID) {
		t.Fatal("unclaimed pairing hidden from second principal:", w.Code)
	}
	decision, _ := json.Marshal(runnerproto.PairingDecision{Decision: "approve", UserCode: unclaimed.UserCode})
	if w := consoleRequest(h, "POST", "/api/me/runner-pairings/"+unclaimed.ID, session, csrf, decision); w.Code != 204 {
		t.Fatal(w.Code, w.Body.String())
	}
	credential := pairedRunner(t, b, principal)
	var record runnerRecord
	if err := h.passport.db.View(func(tx *bbolt.Tx) error {
		return json.Unmarshal(tx.Bucket([]byte(bucketRunners)).Get([]byte(credential.RunnerID)), &record)
	}); err != nil {
		t.Fatal(err)
	}
	rotation := startTestPairing(t, b, &record)
	fresh := startTestPairing(t, b, nil)
	list := consoleRequest(h, "GET", "/api/me/runner-pairings", session, "", nil)
	if list.Code != 200 {
		t.Fatal(list.Code, list.Body.String())
	}
	var body struct {
		Pairings []runnerproto.PairingReview `json:"pairings"`
	}
	if err := json.Unmarshal(list.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	want := []string{rotation.ID, fresh.ID}
	if cmp.Or(fresh.ExpiresAt.Compare(rotation.ExpiresAt), strings.Compare(fresh.ID, rotation.ID)) < 0 {
		want = []string{fresh.ID, rotation.ID}
	}
	if len(body.Pairings) != 2 || body.Pairings[0].ID != want[0] || body.Pairings[1].ID != want[1] {
		t.Fatalf("visibility or expiry sort wrong: %+v", body.Pairings)
	}
	for _, secret := range []string{unclaimed.UserCode, unclaimed.DeviceSecret, fresh.UserCode, fresh.DeviceSecret, "secret_digest", "code_digest", "user_code"} {
		if strings.Contains(list.Body.String(), secret) {
			t.Fatalf("pairing list leaked %q", secret)
		}
	}
	otherList := consoleRequest(h, "GET", "/api/me/runner-pairings", otherSession, "", nil)
	if otherList.Code != 200 || strings.Contains(otherList.Body.String(), rotation.ID) || !strings.Contains(otherList.Body.String(), fresh.ID) {
		t.Fatal("claimed rotation leaked:", otherList.Body.String())
	}
	b.mu.Lock()
	realNow := b.now
	b.now = func() time.Time { return realNow().Add(10 * time.Minute) }
	b.mu.Unlock()
	expired := consoleRequest(h, "GET", "/api/me/runner-pairings", session, "", nil)
	if expired.Code != 200 || !strings.Contains(expired.Body.String(), `"pairings":[]`) {
		t.Fatal("expired pairings not pruned:", expired.Body.String())
	}
	if err := h.passport.db.View(func(tx *bbolt.Tx) error {
		if n := tx.Bucket([]byte(bucketRunnerPairings)).Stats().KeyN; n != 0 {
			t.Fatalf("expired pairings left in bucket: %d", n)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	b.mu.Lock()
	b.now = realNow
	b.mu.Unlock()
}
