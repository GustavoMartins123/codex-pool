package main

import (
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"net"
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

	// Attacker tries to mint setup links for victim's client
	{
		req := httptest.NewRequest(http.MethodPost, "/api/me/clients/"+clientVictim.ID+"/setup-link", nil)
		req.AddCookie(&http.Cookie{Name: "pool_session", Value: attackerToken})
		req.AddCookie(&http.Cookie{Name: "pool_csrf", Value: attackerCsrf})
		req.Header.Set("X-CSRF-Token", attackerCsrf)
		rr := httptest.NewRecorder()
		h.handlePassportClientItem(rr, req)

		if rr.Code != http.StatusNotFound && rr.Code != http.StatusForbidden {
			t.Fatalf("minting setup links for another member's client should fail (404/403), got %d: %s", rr.Code, rr.Body.String())
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

// TestSecurityClientIPHeaderSpoofing verifies that untrusted remote peers cannot spoof
// their client IP via proxy headers (CF-Connecting-IP, X-Forwarded-For, X-Real-IP),
// preventing account whitelist bypass, effort cap evasion, and brute-force ban framing.
func TestSecurityClientIPHeaderSpoofing(t *testing.T) {
	// Reset trusted proxies to default (only loopback trusted)
	setTrustedProxies(nil)

	// 1. Untrusted remote peer (e.g. 203.0.113.50) cannot spoof IP via proxy headers
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.RemoteAddr = "203.0.113.50:12345"

	if ip := getClientIP(req); ip != "203.0.113.50" {
		t.Fatalf("expected remote addr 203.0.113.50, got %s", ip)
	}

	// Direct untrusted peer tries CF-Connecting-IP
	req.Header.Set("CF-Connecting-IP", "199.45.144.95")
	if ip := getClientIP(req); ip != "203.0.113.50" {
		t.Fatalf("expected untrusted peer CF-Connecting-IP to be ignored, got %s", ip)
	}

	// Direct untrusted peer tries X-Forwarded-For
	req.Header.Del("CF-Connecting-IP")
	req.Header.Set("X-Forwarded-For", "198.51.100.77, 10.0.0.1")
	if ip := getClientIP(req); ip != "203.0.113.50" {
		t.Fatalf("expected untrusted peer XFF to be ignored, got %s", ip)
	}

	// Direct untrusted peer tries X-Real-IP
	req.Header.Del("X-Forwarded-For")
	req.Header.Set("X-Real-IP", "198.51.100.88")
	if ip := getClientIP(req); ip != "203.0.113.50" {
		t.Fatalf("expected untrusted peer X-Real-IP to be ignored, got %s", ip)
	}

	// 2. Untrusted client attempting to bypass Account.AllowedSourceIPs is rejected
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

	// Attacker sends X-Forwarded-For: 198.51.100.77, but getClientIP returns real IP 203.0.113.50
	req.Header.Set("X-Forwarded-For", "198.51.100.77")
	clientIP := getClientIP(req)
	cand := p.candidate("", nil, AccountTypeCodex, "", clientIP)
	if cand == nil || cand.ID != "public-fallback-account" {
		t.Fatalf("untrusted client should not access restricted account via spoofed header, got %+v", cand)
	}

	// 3. Untrusted client cannot evade origin-based reasoning effort caps
	cap := newEffortCap(nil, map[string]string{"203.0.113.50": "low"})
	if limit := cap.limitFor("unrestricted-user", clientIP); limit != "low" {
		t.Fatalf("effort cap for physical origin 203.0.113.50 must be enforced, got %q", limit)
	}

	// 4. Untrusted client cannot evade brute-force tracking by rotating headers
	bf := newBruteForceTracker()
	defer bf.stop()

	// Attacker makes 5 failed attempts from 203.0.113.50 with rotating spoofed XFF headers
	for i := 0; i < 5; i++ {
		req.Header.Set("X-Forwarded-For", fmt.Sprintf("185.220.101.%d", i))
		bf.recordFailure(getClientIP(req))
	}
	// Attacker's real IP 203.0.113.50 is banned!
	if !bf.isBanned("203.0.113.50") {
		t.Fatal("attacker real IP 203.0.113.50 should have been banned despite rotating XFF headers")
	}

	// Victim IP cannot be framed by untrusted peer sending spoofed headers
	victimIP := "192.0.2.100"
	if bf.isBanned(victimIP) {
		t.Fatalf("victim IP %s was framed and banned!", victimIP)
	}

	// 5. Trusted proxy (e.g. loopback) properly forwards client IP
	trustedReq := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	trustedReq.RemoteAddr = "127.0.0.1:44321" // Loopback proxy (Caddy/Nginx)
	trustedReq.Header.Set("X-Forwarded-For", "198.51.100.77, 127.0.0.1")
	if ip := getClientIP(trustedReq); ip != "198.51.100.77" {
		t.Fatalf("trusted proxy XFF should be respected, got %s", ip)
	}

	trustedReq.Header.Set("CF-Connecting-IP", "199.45.144.95")
	if ip := getClientIP(trustedReq); ip != "199.45.144.95" {
		t.Fatalf("trusted proxy CF-Connecting-IP should take precedence, got %s", ip)
	}

	// 6. Explicitly configured trusted proxy CIDR
	setTrustedProxies([]string{"10.0.0.0/8"})
	defer setTrustedProxies(nil)

	cidrReq := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	cidrReq.RemoteAddr = "10.244.0.15:55123"
	cidrReq.Header.Set("X-Real-IP", "203.0.113.99")
	if ip := getClientIP(cidrReq); ip != "203.0.113.99" {
		t.Fatalf("trusted CIDR proxy X-Real-IP should be respected, got %s", ip)
	}
}

func TestIPSharesSubnetWithAny(t *testing.T) {
	eth0 := &net.IPNet{IP: net.ParseIP("172.18.0.5").To4(), Mask: net.CIDRMask(16, 32)}
	if !ipSharesSubnetWithAny(net.ParseIP("172.18.0.9"), []*net.IPNet{eth0}) {
		t.Fatal("same /16 subnet peer must match")
	}
	if ipSharesSubnetWithAny(net.ParseIP("172.19.0.9"), []*net.IPNet{eth0}) {
		t.Fatal("peer outside the /16 must not match")
	}
	if ipSharesSubnetWithAny(nil, []*net.IPNet{eth0}) {
		t.Fatal("nil IP must not match")
	}
	if ipSharesSubnetWithAny(net.ParseIP("172.18.0.9"), nil) {
		t.Fatal("no subnets must never match")
	}
	ula := &net.IPNet{IP: net.ParseIP("fd00::1"), Mask: net.CIDRMask(64, 128)}
	if !ipSharesSubnetWithAny(net.ParseIP("fd00::42"), []*net.IPNet{ula}) {
		t.Fatal("same IPv6 /64 peer must match")
	}
	if ipSharesSubnetWithAny(net.ParseIP("172.18.0.9"), []*net.IPNet{ula}) {
		t.Fatal("IPv4 peer must not match an IPv6 subnet")
	}
}

// TestSecuritySameSubnetProxyTrust verifies the generic container-network
// proxy trust: a reverse proxy sharing a subnet with this process is trusted
// (so its X-Forwarded-For is honored) while unrelated peers stay untrusted,
// and the mode is strictly opt-in via PROXY_TRUST_SAME_SUBNET.
func TestSecuritySameSubnetProxyTrust(t *testing.T) {
	setTrustedProxies(nil)
	setTrustSameSubnet(false)

	var subnet *net.IPNet
	for _, candidate := range localInterfaceSubnets() {
		if candidate.IP.To4() != nil {
			subnet = candidate
			break
		}
	}
	if subnet == nil {
		t.Skip("no IPv4 interface subnet available")
	}
	peer := append(net.IP(nil), subnet.IP.To4()...)
	peer[3]++
	if !subnet.Contains(peer) {
		t.Skip("interface subnet too small to derive a neighbor IP")
	}
	outside := make(net.IP, 4)
	binary.BigEndian.PutUint32(outside, binary.BigEndian.Uint32(subnet.IP.To4())^0xFFFFFFFF)

	// Opt-in disabled (default): same-subnet peer headers are ignored.
	req := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	req.RemoteAddr = net.JoinHostPort(peer.String(), "40000")
	req.Header.Set("X-Forwarded-For", "198.51.100.77")
	if ip := getClientIP(req); ip != peer.String() {
		t.Fatalf("same-subnet peer must not be trusted by default, got %s", ip)
	}

	setTrustSameSubnet(true)
	defer setTrustSameSubnet(false)

	if ip := getClientIP(req); ip != "198.51.100.77" {
		t.Fatalf("same-subnet proxy XFF should be respected, got %s", ip)
	}

	outsider := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	outsider.RemoteAddr = net.JoinHostPort(outside.String(), "40000")
	outsider.Header.Set("X-Forwarded-For", "198.51.100.77")
	if ip := getClientIP(outsider); ip != outside.String() {
		t.Fatalf("peer outside local subnets must stay untrusted, got %s", ip)
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
// pinning is keyed with user/principal scoping, preventing User B from latching
// onto User A's pinned upstream account even if they send the same conversation ID.
func TestSecurityConversationPinningCrossUserInterference(t *testing.T) {
	acc1 := &Account{ID: "acc-pinned-1", Type: AccountTypeCodex, PlanType: "pro", Usage: UsageSnapshot{PrimaryUsedPercent: 0.9}}
	acc2 := &Account{ID: "acc-pinned-2", Type: AccountTypeCodex, PlanType: "pro", Usage: UsageSnapshot{PrimaryUsedPercent: 0.1}}
	pool := newPoolState([]*Account{acc1, acc2}, false)

	sharedConvID := "shared-thread-uuid-12345"
	userA := "user_alice"
	userB := "user_bob"

	// User A pins their conversation to acc-pinned-1
	pool.pinForUser(userA, sharedConvID, acc1.ID)

	// User A's subsequent requests route to the pinned account acc-pinned-1
	candA := pool.candidateForUser(userA, sharedConvID, nil, AccountTypeCodex, "", "198.51.100.1")
	if candA == nil || candA.ID != "acc-pinned-1" {
		t.Fatalf("User A should route to pinned account acc-pinned-1, got %+v", candA)
	}

	// User B sends a request with the exact same session_id header
	reqUserB := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	reqUserB.Header.Set("session_id", sharedConvID)
	extractedB := extractConversationIDFromHeaders(reqUserB.Header)
	if extractedB != sharedConvID {
		t.Fatalf("expected extracted conversation ID %s, got %s", sharedConvID, extractedB)
	}

	// User B does NOT get forced onto User A's heavily used acc-pinned-1 (0.9 vs 0.1)
	candB := pool.candidateForUser(userB, extractedB, nil, AccountTypeCodex, "", "198.51.100.2")
	if candB == nil || candB.ID != "acc-pinned-2" {
		t.Fatalf("User B should route to best available account (acc-pinned-2), not User A's pinned account, got %+v", candB)
	}
}

// TestSecurityUnauthenticatedPassthroughOpenProxy demonstrates that unauthenticated
// clients cannot use provider credentials (sk-, sk-ant-, ya29.) to exploit the server
// as an open egress proxy, while valid pool users carrying a pool token can.
func TestSecurityUnauthenticatedPassthroughOpenProxy(t *testing.T) {
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "test-passport-encryption-key")
	s := testUsageStore(t)
	p, err := newPassportStore(s.db, nil)
	if err != nil {
		t.Fatal(err)
	}

	h := &proxyHandler{
		cfg:      &config{},
		passport: p,
		metrics:  newMetrics(),
		pool:     newPoolState(nil, false),
	}

	// 1. Unauthenticated attacker sending Bearer sk-ant-... is rejected with 401
	reqUnauth := httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{}`))
	reqUnauth.Header.Set("Authorization", "Bearer sk-ant-api03-external-anthropic-key")
	rrUnauth := httptest.NewRecorder()
	h.proxyRequest(rrUnauth, reqUnauth, "req-unauth-test")

	if rrUnauth.Code != http.StatusUnauthorized {
		t.Fatalf("unauthenticated passthrough should be 401, got %d: %s", rrUnauth.Code, rrUnauth.Body.String())
	}
	if !strings.Contains(rrUnauth.Body.String(), "valid pool credential") {
		t.Fatalf("expected error message to require valid pool credential, got: %s", rrUnauth.Body.String())
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

