package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

// TestSecurityPrivilegeEscalation verifies that a non-operator (member or guest)
// cannot elevate their role, alter effort caps, or perform administrative operations.
func TestSecurityPrivilegeEscalation(t *testing.T) {
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "test-passport-encryption-key")
	s := testUsageStore(t)
	p, err := newPassportStore(s.db, nil)
	if err != nil {
		t.Fatal(err)
	}

	// 1. Create an operator and a standard member
	guest, _, _, _, err := p.createGuest("bootstrap", "Operator", "Operator", nil)
	if err != nil {
		t.Fatal(err)
	}
	operator, err := p.setPrincipalKind("bootstrap", guest.ID, PrincipalOperator)
	if err != nil {
		t.Fatal(err)
	}

	member := insertTestPrincipal(t, p, "member-victim", PrincipalMember, "victim", "victim@pool.local")
	attacker := insertTestPrincipal(t, p, "member-attacker", PrincipalMember, "attacker", "attacker@pool.local")

	memberToken, memberCsrf, err := p.createSession(attacker.ID)
	if err != nil {
		t.Fatal(err)
	}

	cap := newEffortCap(nil, nil)
	h := &proxyHandler{
		cfg:       &config{},
		passport:  p,
		effortCap: cap,
		metrics:   newMetrics(),
		pool:      newPoolState(nil, false),
	}

	// Scenario A: Member tries to promote themselves to Operator
	{
		req := httptest.NewRequest(http.MethodPatch, "/api/principals/"+attacker.ID, strings.NewReader(`{"kind":"operator"}`))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: "pool_session", Value: memberToken})
		req.AddCookie(&http.Cookie{Name: "pool_csrf", Value: memberCsrf})
		req.Header.Set("X-CSRF-Token", memberCsrf)
		rr := httptest.NewRecorder()
		h.handlePrincipalItem(rr, req)

		if rr.Code != http.StatusForbidden {
			t.Fatalf("privilege escalation should be 403, got %d: %s", rr.Code, rr.Body.String())
		}
		if got := p.principal(attacker.ID).Kind; got != PrincipalMember {
			t.Fatalf("attacker was promoted to %q!", got)
		}
	}

	// Scenario B: Member tries to change their own or another member's reasoning effort cap
	{
		req := httptest.NewRequest(http.MethodPatch, "/api/principals/"+attacker.ID, strings.NewReader(`{"max_reasoning_effort":""}`))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: "pool_session", Value: memberToken})
		req.AddCookie(&http.Cookie{Name: "pool_csrf", Value: memberCsrf})
		req.Header.Set("X-CSRF-Token", memberCsrf)
		rr := httptest.NewRecorder()
		h.handlePrincipalItem(rr, req)

		if rr.Code != http.StatusForbidden {
			t.Fatalf("unauthorized effort cap change should be 403, got %d", rr.Code)
		}
	}

	// Scenario C: Member tries to suspend another member
	{
		req := httptest.NewRequest(http.MethodPatch, "/api/principals/"+member.ID, strings.NewReader(`{"status":"suspended"}`))
		req.Header.Set("Content-Type", "application/json")
		req.AddCookie(&http.Cookie{Name: "pool_session", Value: memberToken})
		req.AddCookie(&http.Cookie{Name: "pool_csrf", Value: memberCsrf})
		req.Header.Set("X-CSRF-Token", memberCsrf)
		rr := httptest.NewRecorder()
		h.handlePrincipalItem(rr, req)

		if rr.Code != http.StatusForbidden {
			t.Fatalf("unauthorized suspension should be 403, got %d", rr.Code)
		}
		if got := p.principal(member.ID).Status; got != PrincipalActive {
			t.Fatalf("victim status changed to %q", got)
		}
	}

	// Scenario D: Operator cannot demote the last remaining operator
	{
		_, err := p.setPrincipalKind(operator.ID, operator.ID, PrincipalMember)
		if err == nil {
			t.Fatal("demoting the last operator should have failed, but succeeded")
		}
		if !strings.Contains(err.Error(), "last operator") {
			t.Fatalf("expected 'last operator' error, got %v", err)
		}
	}
}

// TestSecurityModelPolicyEnforcement verifies that client model policy restrictions
// cannot be bypassed by requesting unauthorized models, model aliases, or suffixes.
func TestSecurityModelPolicyEnforcement(t *testing.T) {
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "test-passport-encryption-key")
	s := testUsageStore(t)
	p, err := newPassportStore(s.db, nil)
	if err != nil {
		t.Fatal(err)
	}

	// Client policy restricting models to ONLY gpt-5.5
	policies := map[string]ClientPolicy{
		"restricted-client": {
			Models: PolicySelector{
				Allow: []string{"gpt-5.5"},
				Deny:  []string{"gpt-6-astra", "claude*"},
			},
		},
	}

	member := insertTestPrincipal(t, p, "principal-restricted", PrincipalMember, "restricted", "res@pool.local")

	// Create client credential with restricted policy
	client := &ClientCredential{
		ID:          "restricted-client",
		PrincipalID: member.ID,
		Label:       "restricted-client",
		Status:      "active",
		CreatedAt:   time.Now().UTC(),
		Policy:      policies["restricted-client"],
	}
	_ = p.db.Update(func(tx *bbolt.Tx) error {
		return putJSON(tx.Bucket([]byte(bucketClientCredentials)), client.ID, client)
	})
	p.mu.Lock()
	p.clients[client.ID] = client
	p.mu.Unlock()

	// Check model admission
	admission, err := p.beginPolicyRequest(member.ID, client.ID, policies, time.Now())
	if err != nil {
		t.Fatalf("beginPolicyRequest failed: %v", err)
	}
	defer admission.Release()

	// Allowed model must pass
	if err := admission.CheckModel("gpt-5.5"); err != nil {
		t.Fatalf("allowed model gpt-5.5 was rejected: %v", err)
	}

	// Denied model explicitly in deny list must be rejected
	if err := admission.CheckModel("gpt-6-astra"); err == nil {
		t.Fatal("explicitly denied model gpt-6-astra was allowed!")
	} else {
		pe, ok := err.(*policyError)
		if !ok || pe.Code != "policy_model_denied" {
			t.Fatalf("expected policy_model_denied error, got %v", err)
		}
	}

	// Unlisted model not in allow list must be rejected
	if err := admission.CheckModel("gpt-4o"); err == nil {
		t.Fatal("unlisted model gpt-4o was allowed when allow list is present!")
	}

	// Wildcard deny model (claude*) must be rejected
	if err := admission.CheckModel("claude-opus-4-6"); err == nil {
		t.Fatal("wildcard denied claude model was allowed!")
	}
}

// TestSecurityConversationDataIsolation verifies that one user cannot snoop on
// or read debug conversation metadata of other users or access internal admin inspection.
func TestSecurityConversationDataIsolation(t *testing.T) {
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "test-passport-encryption-key")
	s := testUsageStore(t)
	p, err := newPassportStore(s.db, nil)
	if err != nil {
		t.Fatal(err)
	}

	member := insertTestPrincipal(t, p, "member-attacker", PrincipalMember, "attacker", "attacker@pool.local")
	memberToken, memberCsrf, err := p.createSession(member.ID)
	if err != nil {
		t.Fatal(err)
	}

	h := &proxyHandler{
		cfg:      &config{adminToken: "super-secret-admin-token"},
		passport: p,
		metrics:  newMetrics(),
		pool:     newPoolState(nil, false),
	}

	// Scenario A: Member tries to read conversation debug inspection without admin token or operator role
	{
		req := httptest.NewRequest(http.MethodGet, "/admin/debug/conversations/victim-conversation-12345", nil)
		req.AddCookie(&http.Cookie{Name: "pool_session", Value: memberToken})
		req.AddCookie(&http.Cookie{Name: "pool_csrf", Value: memberCsrf})
		req.Header.Set("X-CSRF-Token", memberCsrf)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)

		if rr.Code != http.StatusUnauthorized && rr.Code != http.StatusForbidden {
			t.Fatalf("unauthorized conversation debug inspection should be 401/403, got %d", rr.Code)
		}
	}

	// Scenario B: Member tries to access global conversation transitions without admin token
	{
		req := httptest.NewRequest(http.MethodGet, "/admin/transitions", nil)
		req.AddCookie(&http.Cookie{Name: "pool_session", Value: memberToken})
		req.AddCookie(&http.Cookie{Name: "pool_csrf", Value: memberCsrf})
		req.Header.Set("X-CSRF-Token", memberCsrf)
		rr := httptest.NewRecorder()
		h.ServeHTTP(rr, req)

		if rr.Code != http.StatusUnauthorized && rr.Code != http.StatusForbidden {
			t.Fatalf("unauthorized transitions log should be 401/403, got %d", rr.Code)
		}
	}

	// Scenario C: Native context scope strictly segments cache and spooling by principal
	{
		scopeA := contextScope("user-alpha-c-client1")
		scopeB := contextScope("user-beta-c-client2")

		if scopeA != "user-alpha" {
			t.Fatalf("expected scope user-alpha, got %q", scopeA)
		}
		if scopeB != "user-beta" {
			t.Fatalf("expected scope user-beta, got %q", scopeB)
		}
		if scopeA == scopeB {
			t.Fatal("context scopes collided across different users!")
		}
	}
}

// TestSecurityClientCredentialIsolation verifies that Member A cannot reveal or rotate
// Member B's client credentials.
func TestSecurityClientCredentialIsolation(t *testing.T) {
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "test-passport-encryption-key")
	s := testUsageStore(t)
	p, err := newPassportStore(s.db, nil)
	if err != nil {
		t.Fatal(err)
	}

	victim := insertTestPrincipal(t, p, "victim", PrincipalMember, "victim", "victim@pool.local")
	attacker := insertTestPrincipal(t, p, "attacker", PrincipalMember, "attacker", "attacker@pool.local")

	// Create a client credential owned by victim
	clientVictim := &ClientCredential{
		ID:          "client-victim-999",
		PrincipalID: victim.ID,
		Label:       "Victim Laptop",
		Status:      "active",
		CreatedAt:   time.Now().UTC(),
	}
	_ = p.db.Update(func(tx *bbolt.Tx) error {
		return putJSON(tx.Bucket([]byte(bucketClientCredentials)), clientVictim.ID, clientVictim)
	})
	p.mu.Lock()
	p.clients[clientVictim.ID] = clientVictim
	p.mu.Unlock()

	// Attacker logs in
	attackerToken, attackerCsrf, err := p.createSession(attacker.ID)
	if err != nil {
		t.Fatal(err)
	}

	h := &proxyHandler{
		cfg:      &config{},
		passport: p,
		metrics:  newMetrics(),
	}

	// Attacker tries to reveal victim's credential download token
	{
		req := httptest.NewRequest(http.MethodPost, "/api/me/clients/"+clientVictim.ID+"/reveal", nil)
		req.AddCookie(&http.Cookie{Name: "pool_session", Value: attackerToken})
		req.AddCookie(&http.Cookie{Name: "pool_csrf", Value: attackerCsrf})
		req.Header.Set("X-CSRF-Token", attackerCsrf)
		rr := httptest.NewRecorder()
		h.handlePassportClientItem(rr, req)

		if rr.Code != http.StatusNotFound && rr.Code != http.StatusForbidden {
			t.Fatalf("revealing another member's client should fail (404/403), got %d: %s", rr.Code, rr.Body.String())
		}
	}

	// Attacker tries to rotate/steal victim's credential
	{
		req := httptest.NewRequest(http.MethodPost, "/api/me/clients/"+clientVictim.ID+"/rotate", nil)
		req.AddCookie(&http.Cookie{Name: "pool_session", Value: attackerToken})
		req.AddCookie(&http.Cookie{Name: "pool_csrf", Value: attackerCsrf})
		req.Header.Set("X-CSRF-Token", attackerCsrf)
		rr := httptest.NewRecorder()
		h.handlePassportClientItem(rr, req)

		if rr.Code != http.StatusBadRequest && rr.Code != http.StatusNotFound && rr.Code != http.StatusForbidden {
			t.Fatalf("rotating another member's client should fail, got %d: %s", rr.Code, rr.Body.String())
		}
	}
}

// TestSecurityEffortCapBypassDefense verifies that suffix tricks like model="gpt-5.5-xhigh"
// and direct JSON payload manipulations cannot bypass the reasoning effort ceiling.
func TestSecurityEffortCapBypassDefense(t *testing.T) {
	t.Setenv("POOL_JWT_SECRET", "bypass-test-secret")
	const principal = "victim-member-id"
	token := generateClaudePoolToken("bypass-test-secret", principal)

	var interceptedBody []byte
	h := preflightHandler(roundTripFunc(func(req *http.Request) (*http.Response, error) {
		interceptedBody, _ = io.ReadAll(req.Body)
		return preflightReply(), nil
	}))

	// Limit victim to medium
	h.effortCap = newEffortCap(map[string]string{principal: "medium"}, nil)

	// Attempt bypass 1: Sending model with suffix "-xhigh"
	body1 := `{"model":"gpt-5.5-xhigh","stream":true,"reasoning":{"effort":"low"},"input":"test"}`
	req1 := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body1))
	req1.Header.Set("Authorization", "Bearer "+token)
	req1.Header.Set("Content-Type", "application/json")
	h.proxyRequest(httptest.NewRecorder(), req1, "bypass-1")

	var obj1 map[string]any
	if err := json.Unmarshal(interceptedBody, &obj1); err != nil {
		t.Fatalf("unmarshal intercepted: %v", err)
	}
	// Model suffix was stripped and reasoning effort clamped
	if obj1["model"] != "gpt-5.5" {
		t.Fatalf("model = %v, want gpt-5.5", obj1["model"])
	}
	reasoning1 := obj1["reasoning"].(map[string]any)
	if reasoning1["effort"] != "medium" {
		t.Fatalf("effort = %v, want medium", reasoning1["effort"])
	}

	// Attempt bypass 2: Raw xhigh request
	body2 := `{"model":"gpt-5.5","stream":true,"reasoning":{"effort":"xhigh"},"input":"test"}`
	req2 := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body2))
	req2.Header.Set("Authorization", "Bearer "+token)
	req2.Header.Set("Content-Type", "application/json")
	h.proxyRequest(httptest.NewRecorder(), req2, "bypass-2")

	var obj2 map[string]any
	if err := json.Unmarshal(interceptedBody, &obj2); err != nil {
		t.Fatalf("unmarshal intercepted: %v", err)
	}
	reasoning2 := obj2["reasoning"].(map[string]any)
	if reasoning2["effort"] != "medium" {
		t.Fatalf("effort = %v, want medium", reasoning2["effort"])
	}
}
