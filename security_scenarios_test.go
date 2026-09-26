package main

import (
	"encoding/json"
	"fmt"
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

// TestSecurityClientIPHeaderSpoofing demonstrates how trusting untrusted proxy headers
// (CF-Connecting-IP, X-Forwarded-For, X-Real-IP) allows clients to bypass IP filters,
// evade effort caps, circumvent brute-force tracking, and frame/DoS victim IPs.
func TestSecurityClientIPHeaderSpoofing(t *testing.T) {
	// 1. IP extraction directly trusts headers over RemoteAddr
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.RemoteAddr = "203.0.113.50:12345"

	if ip := getClientIP(req); ip != "203.0.113.50" {
		t.Fatalf("expected remote addr 203.0.113.50, got %s", ip)
	}

	req.Header.Set("CF-Connecting-IP", "199.45.144.95")
	if ip := getClientIP(req); ip != "199.45.144.95" {
		t.Fatalf("expected CF-Connecting-IP spoof to yield 199.45.144.95, got %s", ip)
	}

	req.Header.Del("CF-Connecting-IP")
	req.Header.Set("X-Forwarded-For", "198.51.100.77, 10.0.0.1")
	if ip := getClientIP(req); ip != "198.51.100.77" {
		t.Fatalf("expected XFF spoof to yield 198.51.100.77, got %s", ip)
	}

	// 2. Spoofed IP bypasses Account.AllowedSourceIPs protection
	restricted := &Account{
		ID:               "restricted-corp-account",
		Type:             AccountTypeCodex,
		PlanType:         "pro",
		AllowedSourceIPs: []string{"198.51.100.77"},
		Usage:            UsageSnapshot{PrimaryUsedPercent: 0.1},
	}
	fallback := &Account{
		ID:       "public-fallback-account",
		Type:     AccountTypeCodex,
		PlanType: "pro",
		Usage:    UsageSnapshot{PrimaryUsedPercent: 0.2},
	}
	p := newPoolState([]*Account{restricted, fallback}, false)

	// Legitimate client from 203.0.113.50 is denied restricted account
	legitCand := p.candidate("", nil, AccountTypeCodex, "", "203.0.113.50")
	if legitCand == nil || legitCand.ID != "public-fallback-account" {
		t.Fatalf("legitimate non-whitelisted IP should get public account, got %+v", legitCand)
	}

	// Spoofing client supplying XFF: 198.51.100.77 accesses restricted account
	spoofedIP := getClientIP(req)
	spoofedCand := p.candidate("", nil, AccountTypeCodex, "", spoofedIP)
	if spoofedCand == nil || spoofedCand.ID != "restricted-corp-account" {
		t.Fatalf("spoofed client IP should have bypassed filter and accessed restricted account, got %+v", spoofedCand)
	}

	// 3. Spoofed IP evades origin-based reasoning effort caps
	// Administrator configured an effort cap for the physical origin 203.0.113.50
	cap := newEffortCap(nil, map[string]string{"203.0.113.50": "low"})
	if limit := cap.limitFor("unrestricted-user", "203.0.113.50"); limit != "low" {
		t.Fatalf("cap for 203.0.113.50 should be low, got %q", limit)
	}
	// By forging X-Forwarded-For, the client completely evades the cap
	if limit := cap.limitFor("unrestricted-user", spoofedIP); limit != "" {
		t.Fatalf("spoofed IP %s should have bypassed the origin cap, got %q", spoofedIP, limit)
	}

	// 4. Spoofed IP evades brute-force tracking & enables Denial-of-Service / IP framing
	bf := newBruteForceTracker()
	defer bf.stop()

	// Attacker rotates X-Forwarded-For: attacker real IP is never tracked/banned
	for i := 0; i < 10; i++ {
		fakeIP := fmt.Sprintf("185.220.101.%d", i)
		bf.recordFailure(fakeIP)
	}
	if bf.isBanned("203.0.113.50") {
		t.Fatal("attacker real IP was unexpectedly banned despite rotating spoofed XFF headers")
	}

	// Attacker frames victim IP by sending 5 failures with victim's IP in XFF
	victimIP := "192.0.2.100"
	for i := 0; i < bruteForceMaxAttempts; i++ {
		bf.recordFailure(victimIP)
	}
	if !bf.isBanned(victimIP) {
		t.Fatalf("victim IP %s was not banned as expected in framing scenario", victimIP)
	}
}

// TestSecurityInternalHeaderTampering demonstrates how untrusted incoming headers
// (X-Pool-Canary-Bypass, X-Pool-Routing) manipulate internal routing behavior.
func TestSecurityInternalHeaderTampering(t *testing.T) {
	h := &proxyHandler{
		cfg: &config{adminToken: "valid-admin-secret"},
	}

	// 1. Canary bypass via X-Pool-Canary-Bypass is rejected for non-admin/non-operator
	reqUser := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	reqUser.Header.Set("X-Pool-Canary-Bypass", "1")
	if h.isOperatorOrAdmin(reqUser) {
		t.Fatal("unauthenticated request should not be considered operator or admin")
	}

	// But is allowed when X-Admin-Token is provided
	reqAdmin := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	reqAdmin.Header.Set("X-Pool-Canary-Bypass", "1")
	reqAdmin.Header.Set("X-Admin-Token", "valid-admin-secret")
	if !h.isOperatorOrAdmin(reqAdmin) {
		t.Fatal("admin request should be allowed operator/admin privileges")
	}

	// 2. Client forging X-Codex-Pool-Image-Fanout is rejected without valid token
	reqFakeFanout := httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	reqFakeFanout.Header.Set("X-Codex-Pool-Image-Fanout", "1")
	if h.isValidImageFanout(reqFakeFanout) {
		t.Fatal("client request forging X-Codex-Pool-Image-Fanout without token must be rejected")
	}

	// Valid internal fanout request carrying the token is accepted
	reqValidFanout := httptest.NewRequest(http.MethodPost, "/v1/images/generations", nil)
	reqValidFanout.Header.Set("X-Codex-Pool-Image-Fanout", "1")
	reqValidFanout.Header.Set("X-Codex-Pool-Image-Fanout-Token", h.getFanoutToken())
	if !h.isValidImageFanout(reqValidFanout) {
		t.Fatal("internal fanout with valid token must be accepted")
	}

	// 3. Client-specified X-Pool-Routing override
	pool := newPoolState(nil, false)
	reqProfile := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	reqProfile.Header.Set("X-Pool-Routing", "quota-saver")
	profile, ok := pool.resolveRoutingProfile(reqProfile.Header.Get("X-Pool-Routing"))
	if !ok || profile != RoutingQuotaSaver {
		t.Fatalf("expected quota-saver profile resolution, got %v, ok=%v", profile, ok)
	}
}

// TestSecurityConversationPinningCrossUserInterference demonstrates that conversation
// pinning is keyed globally without user/principal scoping, allowing one client to
// force their traffic onto another user's pinned upstream account.
func TestSecurityConversationPinningCrossUserInterference(t *testing.T) {
	acc1 := &Account{ID: "acc-pinned-1", Type: AccountTypeCodex, PlanType: "pro"}
	acc2 := &Account{ID: "acc-pinned-2", Type: AccountTypeCodex, PlanType: "pro"}
	pool := newPoolState([]*Account{acc1, acc2}, false)

	sharedConvID := "shared-thread-uuid-12345"
	// User A has a conversation pinned to acc-pinned-1
	pool.pin(sharedConvID, acc1.ID)

	// User B sends a request with session_id header matching User A's conversation
	reqUserB := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	reqUserB.Header.Set("session_id", sharedConvID)
	extractedB := extractConversationIDFromHeaders(reqUserB.Header)
	if extractedB != sharedConvID {
		t.Fatalf("expected extracted conversation ID %s, got %s", sharedConvID, extractedB)
	}

	// User B routes to the exact same account pinned by User A
	candB := pool.candidate(extractedB, nil, AccountTypeCodex, "", "198.51.100.2")
	if candB == nil || candB.ID != "acc-pinned-1" {
		t.Fatalf("User B should have collided onto User A's pinned account acc-pinned-1, got %+v", candB)
	}
}

// TestSecurityUnauthenticatedPassthroughOpenProxy demonstrates that arbitrary unauthenticated
// clients sending real or fake provider credentials (sk-, sk-ant-, ya29.) are passed through
// rather than rejected with HTTP 401 Unauthorized.
func TestSecurityUnauthenticatedPassthroughOpenProxy(t *testing.T) {
	testCases := []struct {
		header       string
		wantProvider AccountType
	}{
		{"Bearer sk-proj-external-openai-key-from-attacker", AccountTypeCodex},
		{"Bearer sk-ant-api03-external-anthropic-key", AccountTypeClaude},
		{"Bearer ya29.external-google-access-token", AccountTypeGemini},
	}

	for _, tc := range testCases {
		isProvider, providerType := looksLikeProviderCredential(tc.header)
		if !isProvider {
			t.Fatalf("looksLikeProviderCredential(%q) returned false, want true", tc.header)
		}
		if providerType != tc.wantProvider {
			t.Fatalf("provider type for %q = %s, want %s", tc.header, providerType, tc.wantProvider)
		}
	}
}

// TestSecurityUnboundedStainlessTimeout demonstrates that clients supplying an
// arbitrarily large X-Stainless-Timeout are clamped to 10 minutes to prevent resource exhaustion.
func TestSecurityUnboundedStainlessTimeout(t *testing.T) {
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	// Attacker requests 86400 seconds (24 hours) timeout
	req.Header.Set("X-Stainless-Timeout", "86400")

	configuredReqTimeout := 30 * time.Second
	configuredStreamTimeout := 5 * time.Minute

	timeout := timeoutForRequestIntent(req, configuredReqTimeout, configuredStreamTimeout, requestIntent{})
	const expectedCap = 10 * time.Minute
	if timeout != expectedCap {
		t.Fatalf("expected X-Stainless-Timeout to be clamped to %v, got %v", expectedCap, timeout)
	}
}

