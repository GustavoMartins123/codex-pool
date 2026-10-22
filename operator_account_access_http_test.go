package main

import (
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestOperatorAccountAccessHTTPPermissionsAndMinimalDiscovery(t *testing.T) {
	p, pool := ownershipFixture(t)
	h := &proxyHandler{passport: p, pool: pool, cfg: &config{}}
	a := pool.allAccounts()[1]
	alice, aliceCSRF := addTestPassportOperator(t, p, "alice")
	bob, csrf := addTestPassportOperator(t, p, "bob")
	addTestPassportOperator(t, p, "member")
	if _, err := p.setPrincipalKind("alice", "member", PrincipalMember); err != nil {
		t.Fatal(err)
	}
	// Operator demotion invalidates the previous browser session.
	member, memberCSRF, err := p.createSession("member")
	if err != nil {
		t.Fatal(err)
	}
	path := "/api/accounts/codex/private/operator-access"
	call := func(method, path, session, token, body string) *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, passportContributionRequest(method, path, session, token, []byte(body)))
		return w
	}
	for _, session := range []string{"", member} {
		if w := call("GET", "/api/console/operator-accounts?owner_id=alice", session, "", ""); w.Code < 400 {
			t.Fatal("discovery exposed to nonoperator")
		}
	}
	w := call("GET", "/api/console/operator-accounts?owner_id=alice", bob, "", "")
	if w.Code != 200 {
		t.Fatalf("discovery: %d %s", w.Code, w.Body.String())
	}
	var views []map[string]any
	if err := json.Unmarshal(w.Body.Bytes(), &views); err != nil || len(views) != 1 {
		t.Fatalf("views: %s %v", w.Body.String(), err)
	}
	if len(views[0]) != 5 || views[0]["enabled"] != false || views[0]["owner_id"] != "alice" {
		t.Fatalf("excess discovery metadata: %s", w.Body.String())
	}
	if w.Header().Get("Cache-Control") != "no-store" {
		t.Fatal("cacheable discovery")
	}
	if w := call("PUT", path, bob, "", `{"revision":1,"enabled":true,"reason":"Support"}`); w.Code != 403 {
		t.Fatalf("csrf: %d", w.Code)
	}
	if w := call("PUT", path, member, memberCSRF, `{"revision":1,"enabled":true,"reason":"Attack"}`); w.Code != 403 {
		t.Fatalf("member: %d", w.Code)
	}
	for _, body := range []string{
		`{"revision":1,"reason":"Missing enabled"}`,
		`{"revision":1,"enabled":true,"reason":"Attack","operator_id":"alice"}`,
		`{"revision":1,"enabled":true,"reason":"Support"} {}`,
	} {
		if w := call("PUT", path, bob, csrf, body); w.Code != 400 {
			t.Fatalf("invalid body: %d %s", w.Code, w.Body.String())
		}
	}
	w = call("PUT", path, bob, csrf, `{"revision":1,"enabled":true,"reason":"Support investigation"}`)
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"enabled":true`) {
		t.Fatalf("activation: %d %s", w.Code, w.Body.String())
	}
	if w := call("GET", path, bob, "", ""); w.Code != 403 {
		t.Fatal("operator read another operator's history")
	}
	w = call("GET", path, alice, "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"operator_id":"bob"`) || !strings.Contains(w.Body.String(), "Support investigation") {
		t.Fatalf("owner history: %d %s", w.Code, w.Body.String())
	}
	entries, err := p.recentAudit(100)
	if err != nil {
		t.Fatal(err)
	}
	found := false
	for _, e := range entries {
		if e.Action == "account.operator_access_enabled" && e.ActorID == "bob" && e.SubjectID == a.ID {
			found = true
		}
	}
	if !found {
		t.Fatal("activation was not audited")
	}
	a.VerificationURL = "https://provider.example/verify-sensitive"
	w = call("GET", "/admin/accounts", bob, "", "")
	if w.Code != 200 || !strings.Contains(w.Body.String(), `"can_manage":false`) || strings.Contains(w.Body.String(), "verify-sensitive") {
		t.Fatalf("activation exposed management: %d %s", w.Code, w.Body.String())
	}
	if w := call("PUT", path, bob, csrf, `{"revision":1,"enabled":false,"reason":"Stale"}`); w.Code != 409 {
		t.Fatalf("revision: %d", w.Code)
	}
	if w := call("PUT", path, bob, csrf, `{"revision":2,"enabled":false,"reason":"Completed"}`); w.Code != 200 {
		t.Fatalf("disable: %d", w.Code)
	}
	if p.authorizeAccount("bob", a, "use") == nil {
		t.Fatal("disable retained access")
	}
	if w := call("POST", "/api/me/accounts/codex/private/withdraw", alice, aliceCSRF, `{"revision":3}`); w.Code != 200 {
		t.Fatalf("withdraw: %d %s", w.Code, w.Body.String())
	}
	if w := call("GET", "/api/console/operator-accounts?owner_id=alice", bob, "", ""); w.Body.String() != "[]\n" && strings.TrimSpace(w.Body.String()) != "[]" {
		t.Fatalf("withdrawn discovery: %s", w.Body.String())
	}
	if w := call("PUT", path, bob, csrf, `{"revision":4,"enabled":true,"reason":"Withdrawn"}`); w.Code != 403 {
		t.Fatal("withdrawn account activated")
	}
}

func TestOperatorAccountAccessHTTPRejectsMissingAuthority(t *testing.T) {
	h := &proxyHandler{cfg: &config{}}
	w := httptest.NewRecorder()
	h.serveAccounts(w, httptest.NewRequest("GET", "/admin/accounts", nil))
	if w.Code != 503 {
		t.Fatalf("admin listing without authority: %d", w.Code)
	}
	for _, path := range []string{"/api/console/operator-accounts?owner_id=alice", "/api/accounts/codex/private/operator-access", "/api/console/account-contribution"} {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, httptest.NewRequest("GET", path, nil))
		if w.Code != 503 {
			t.Fatalf("missing authority: %s %d", path, w.Code)
		}
	}
}
