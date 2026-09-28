package main

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

type authoritySession struct {
	token string
	csrf  string
}

func TestSelfAvatarAndClientCatalogAuthority(t *testing.T) {
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "test-passport-encryption-key")
	store := testUsageStore(t)
	passport, err := newPassportStore(store.db)
	if err != nil {
		t.Fatal(err)
	}
	member := addAuthorityPrincipal(t, passport, "member", PrincipalMember)
	operator := addAuthorityPrincipal(t, passport, "operator", PrincipalOperator)
	h := &proxyHandler{cfg: &config{}, passport: passport, pool: newPoolState([]*Account{{ID: "codex-one", Type: AccountTypeCodex}}, false)}
	for _, test := range []struct {
		session authoritySession
		want    int
	}{
		{member, http.StatusForbidden}, {operator, http.StatusNotFound},
	} {
		response := httptest.NewRecorder()
		h.ServeHTTP(response, authorityRequest(http.MethodGet, "/api/avatars/operator", test.session, ""))
		if response.Code != test.want {
			t.Fatalf("avatar status = %d, want %d", response.Code, test.want)
		}
	}
	response := httptest.NewRecorder()
	h.ServeHTTP(response, authorityRequest(http.MethodGet, "/api/pool/catalog", member, ""))
	if response.Code != http.StatusOK {
		t.Fatalf("member catalog = %d", response.Code)
	}
	var body struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Models) == 0 {
		t.Fatal("model discovery returned no models")
	}
	for _, model := range body.Models {
		for _, field := range []string{"supporting_accounts", "available_accounts", "quota_remaining_fraction", "next_reset_at"} {
			if _, exposed := model[field]; exposed {
				t.Fatalf("member catalog exposed %s", field)
			}
		}
	}
}

func TestPoolCredentialModelDiscoveryOmitsCapacity(t *testing.T) {
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "test-passport-encryption-key")
	t.Setenv("POOL_JWT_SECRET", "test-secret-for-model-discovery")
	store := testUsageStore(t)
	passport, err := newPassportStore(store.db)
	if err != nil {
		t.Fatal(err)
	}
	principal, _, client, _, err := passport.createGuest("operator", "Guest", "Guest", nil)
	if err != nil {
		t.Fatal(err)
	}
	identity := principal.ID + "-c-" + client.ID
	h := &proxyHandler{cfg: &config{}, passport: passport, metrics: newMetrics(), pool: newPoolState([]*Account{{ID: "codex-one", Type: AccountTypeCodex}}, false)}
	request := httptest.NewRequest(http.MethodGet, "/api/pool/models", nil)
	request.Header.Set("Authorization", "Bearer "+generateClaudePoolToken(getPoolJWTSecret(), identity))
	response := httptest.NewRecorder()
	h.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("model discovery = %d: %s", response.Code, response.Body.String())
	}
	var body struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Models) == 0 {
		t.Fatal("model discovery returned no models")
	}
	for _, model := range body.Models {
		if _, exposed := model["supporting_accounts"]; exposed {
			t.Fatal("CLI model discovery exposed account counts")
		}
		if _, exposed := model["available_accounts"]; exposed {
			t.Fatal("CLI model discovery exposed available account counts")
		}
	}
}

func TestPoolCredentialRequiresLivePassportState(t *testing.T) {
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "test-passport-encryption-key")
	t.Setenv("POOL_JWT_SECRET", "test-live-passport-secret")
	store := testUsageStore(t)
	passport, err := newPassportStore(store.db)
	if err != nil {
		t.Fatal(err)
	}
	principal, _, client, _, err := passport.createGuest("operator", "Guest", "Guest", nil)
	if err != nil {
		t.Fatal(err)
	}
	identity := principal.ID + "-c-" + client.ID
	request := func(id string) *http.Request {
		r := httptest.NewRequest(http.MethodGet, "/api/pool/models", nil)
		r.Header.Set("Authorization", "Bearer "+generateClaudePoolToken(getPoolJWTSecret(), id))
		return r
	}
	without := &proxyHandler{cfg: &config{}, metrics: newMetrics(), pool: newPoolState(nil, false)}
	if _, _, _, _, allowed := without.authorizePoolCredentialRequest(request(identity)); allowed {
		t.Fatal("signed credential was accepted without Passport")
	}
	denied := httptest.NewRecorder()
	without.requirePoolCredential(denied, request(identity))
	if denied.Code != http.StatusUnauthorized {
		t.Fatalf("missing Passport status = %d, want 401", denied.Code)
	}
	proxyDenied := httptest.NewRecorder()
	without.ServeHTTP(proxyDenied, request(identity))
	if proxyDenied.Code != http.StatusForbidden && proxyDenied.Code != http.StatusUnauthorized {
		t.Fatalf("proxy without Passport status = %d, want 401/403", proxyDenied.Code)
	}
	passthrough := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewBufferString(`{"model":"gpt-5"}`))
	passthrough.Header.Set("Authorization", "Bearer sk-proj-untrusted-provider")
	passthroughDenied := httptest.NewRecorder()
	without.ServeHTTP(passthroughDenied, passthrough)
	if passthroughDenied.Code != http.StatusUnauthorized {
		t.Fatalf("passthrough without Passport status = %d, want 401", passthroughDenied.Code)
	}
	h := &proxyHandler{passport: passport, metrics: newMetrics()}
	if _, _, _, _, allowed := h.authorizePoolCredentialRequest(request(identity)); !allowed {
		t.Fatal("active principal and client were rejected")
	}
	if _, _, _, _, allowed := h.authorizePoolCredentialRequest(request(principal.ID + "-c-missing")); allowed {
		t.Fatal("missing client was accepted")
	}
	passport.mu.Lock()
	passport.clients[client.ID].Status = "revoked"
	passport.mu.Unlock()
	if _, _, _, _, allowed := h.authorizePoolCredentialRequest(request(identity)); allowed {
		t.Fatal("revoked client was accepted")
	}
	passport.mu.Lock()
	passport.clients[client.ID].Status = "active"
	passport.principals[principal.ID].Status = PrincipalSuspended
	passport.mu.Unlock()
	if _, _, _, _, allowed := h.authorizePoolCredentialRequest(request(identity)); allowed {
		t.Fatal("suspended principal was accepted")
	}
	past := time.Now().Add(-time.Minute)
	passport.mu.Lock()
	passport.principals[principal.ID].Status = PrincipalActive
	passport.principals[principal.ID].ExpiresAt = &past
	passport.mu.Unlock()
	if _, _, _, _, allowed := h.authorizePoolCredentialRequest(request(identity)); allowed {
		t.Fatal("expired principal was accepted")
	}
}

func TestPoolCredentialCLIUsageDoesNotExposeGlobalTelemetry(t *testing.T) {
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "test-passport-encryption-key")
	t.Setenv("POOL_JWT_SECRET", "test-secret-for-cli-usage")
	store := testUsageStore(t)
	passport, err := newPassportStore(store.db)
	if err != nil {
		t.Fatal(err)
	}
	principal, _, client, _, err := passport.createGuest("operator", "Guest", "Guest", nil)
	if err != nil {
		t.Fatal(err)
	}
	identity := principal.ID + "-c-" + client.ID
	h := &proxyHandler{cfg: &config{}, passport: passport, metrics: newMetrics(), pool: newPoolState([]*Account{{ID: "private-account", Type: AccountTypeCodex}}, false)}
	for _, path := range []string{"/api/codex/usage", "/api/oauth/profile", "/api/oauth/usage"} {
		request := httptest.NewRequest(http.MethodGet, path, nil)
		request.Header.Set("Authorization", "Bearer "+generateClaudePoolToken(getPoolJWTSecret(), identity))
		response := httptest.NewRecorder()
		h.ServeHTTP(response, request)
		if response.Code != http.StatusOK {
			t.Fatalf("CLI %s = %d: %s", path, response.Code, response.Body.String())
		}
		if bytes.Contains(response.Body.Bytes(), []byte("private-account")) || bytes.Contains(response.Body.Bytes(), []byte("total_accounts")) || bytes.Contains(response.Body.Bytes(), []byte(`"pool":`)) || bytes.Contains(response.Body.Bytes(), []byte("pool_stats")) {
			t.Fatalf("CLI %s leaked global pool data: %s", path, response.Body.String())
		}
	}
}

func TestPublicRouterSurfacesDoNotExposeAdminCredentials(t *testing.T) {
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "test-passport-encryption-key")
	store := testUsageStore(t)
	passport, err := newPassportStore(store.db)
	if err != nil {
		t.Fatal(err)
	}
	h := &proxyHandler{cfg: &config{adminToken: "unexposed-admin-secret"}, passport: passport, metrics: newMetrics(), pool: newPoolState([]*Account{{ID: "private-provider-account", Type: AccountTypeCodex}}, false), startTime: time.Now()}
	for _, path := range []string{"/healthz", "/livez", "/readyz", "/api/auth/config", "/api/pool/whoami", "/config/codex/invalid-nonce", "/oauth/token", "/api/auth/login", "/api/auth/join", "/api/auth/recover", "/admin/claude/callback", "/admin/antigravity/callback"} {
		method := http.MethodGet
		if path == "/oauth/token" || path == "/api/auth/login" || path == "/api/auth/join" || path == "/api/auth/recover" {
			method = http.MethodPost
		}
		response := httptest.NewRecorder()
		h.ServeHTTP(response, httptest.NewRequest(method, path, nil))
		if response.Code == http.StatusInternalServerError {
			t.Fatalf("public %s returned 500", path)
		}
		if bytes.Contains(response.Body.Bytes(), []byte("unexposed-admin-secret")) {
			t.Fatalf("public %s leaked admin credential", path)
		}
		if bytes.Contains(response.Body.Bytes(), []byte("private-provider-account")) {
			t.Fatalf("public %s leaked provider account identity", path)
		}
	}
}

func addAuthorityPrincipal(t *testing.T, passport *PassportStore, id string, kind PrincipalKind) authoritySession {
	t.Helper()
	principal := &Principal{ID: id, Kind: kind, Status: PrincipalActive, Username: id, CreatedAt: time.Now().UTC()}
	if err := passport.db.Update(func(tx *bbolt.Tx) error { return putJSON(tx.Bucket([]byte(bucketPrincipals)), id, principal) }); err != nil {
		t.Fatal(err)
	}
	passport.mu.Lock()
	passport.principals[id] = principal
	passport.mu.Unlock()
	token, csrf, err := passport.createSession(id)
	if err != nil {
		t.Fatal(err)
	}
	return authoritySession{token: token, csrf: csrf}
}

func authorityRequest(method, path string, session authoritySession, body string) *http.Request {
	request := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	request.Header.Set("Content-Type", "application/json")
	request.AddCookie(&http.Cookie{Name: "pool_session", Value: session.token})
	request.AddCookie(&http.Cookie{Name: "pool_csrf", Value: session.csrf})
	request.Header.Set("X-CSRF-Token", session.csrf)
	return request
}

func TestAuthorityMatrix(t *testing.T) {
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "test-passport-encryption-key")
	store := testUsageStore(t)
	passport, err := newPassportStore(store.db)
	if err != nil {
		t.Fatal(err)
	}
	sessions := map[PrincipalKind]authoritySession{
		PrincipalGuest:    addAuthorityPrincipal(t, passport, "guest", PrincipalGuest),
		PrincipalMember:   addAuthorityPrincipal(t, passport, "member", PrincipalMember),
		PrincipalOperator: addAuthorityPrincipal(t, passport, "operator", PrincipalOperator),
	}
	handler := &proxyHandler{cfg: &config{}, passport: passport, metrics: newMetrics(), pool: newPoolState(nil, false)}

	tests := []struct {
		name    string
		method  string
		path    string
		body    string
		allowed map[PrincipalKind]bool
	}{
		// Self-service: each role sees only its own Passport resources.
		{"self identity", http.MethodGet, "/api/auth/me", "", map[PrincipalKind]bool{PrincipalGuest: true, PrincipalMember: true, PrincipalOperator: true}},
		{"self profile", http.MethodPatch, "/api/me/profile", `{"nickname":"Self"}`, map[PrincipalKind]bool{PrincipalGuest: true, PrincipalMember: true, PrincipalOperator: true}},
		{"self avatar", http.MethodPut, "/api/me/avatar", "bad image", map[PrincipalKind]bool{PrincipalGuest: true, PrincipalMember: true, PrincipalOperator: true}},
		{"self clients", http.MethodGet, "/api/me/clients", "", map[PrincipalKind]bool{PrincipalGuest: true, PrincipalMember: true, PrincipalOperator: true}},
		{"self usage", http.MethodGet, "/api/me/usage", "", map[PrincipalKind]bool{PrincipalGuest: true, PrincipalMember: true, PrincipalOperator: true}},
		{"own passkeys", http.MethodGet, "/api/me/passkeys", "", map[PrincipalKind]bool{PrincipalMember: true, PrincipalOperator: true}},
		// Passes: member ownership is checked again inside the pass handlers.
		{"guest passes", http.MethodGet, "/api/passes", "", map[PrincipalKind]bool{PrincipalMember: true, PrincipalOperator: true}},
		{"pass item", http.MethodDelete, "/api/passes/nonexistent", "", map[PrincipalKind]bool{PrincipalMember: true, PrincipalOperator: true}},
		// Console and principal management.
		{"console", http.MethodGet, "/api/console/principals", "", map[PrincipalKind]bool{PrincipalOperator: true}},
		{"member creation", http.MethodPost, "/api/console/members", `{"email":"new@example.com","purpose":"onboard"}`, map[PrincipalKind]bool{PrincipalOperator: true}},
		{"console audit", http.MethodGet, "/api/console/audit", "", map[PrincipalKind]bool{PrincipalOperator: true}},
		{"console analytics", http.MethodGet, "/api/console/analytics-health", "", map[PrincipalKind]bool{PrincipalOperator: true}},
		{"console principal usage", http.MethodGet, "/api/console/principals/nonexistent/usage", "", map[PrincipalKind]bool{PrincipalOperator: true}},
		{"principal update", http.MethodPatch, "/api/principals/guest", `{}`, map[PrincipalKind]bool{PrincipalOperator: true}},
		// Model discovery is allowed to members. Exact capacity is sanitized.
		{"model catalog (members and operators)", http.MethodGet, "/api/pool/catalog", "", map[PrincipalKind]bool{PrincipalMember: true, PrincipalOperator: true}},
		// Provider and operational administration.
		{"provider OAuth contribution", http.MethodPost, "/api/pool/accounts/codex/add", `{}`, map[PrincipalKind]bool{PrincipalOperator: true}},
		{"provider key contribution", http.MethodPost, "/api/pool/accounts/kimi/add", `{}`, map[PrincipalKind]bool{PrincipalOperator: true}},
		{"admin accounts", http.MethodGet, "/admin/accounts", "", map[PrincipalKind]bool{PrincipalOperator: true}},
		{"admin mutation", http.MethodPost, "/admin/accounts/nonexistent/disable", "", map[PrincipalKind]bool{PrincipalOperator: true}},
	}
	// Global telemetry is operator-only. The catalog remains available to
	// members for model discovery and omits administrative capacity details.
	for _, path := range []string{
		"/api/pool/stats", "/api/pool/performance", "/api/pool/circuit-breakers",
		"/api/pool/users", "/api/pool/origins", "/api/pool/daily-breakdown",
		"/api/pool/hourly", "/api/pool/signal", "/api/pool/experiments", "/api/pool/users/foo/daily",
		"/api/pool/users/foo/hourly", "/status",
	} {
		tests = append(tests, struct {
			name    string
			method  string
			path    string
			body    string
			allowed map[PrincipalKind]bool
		}{"global " + path, http.MethodGet, path, "", map[PrincipalKind]bool{PrincipalOperator: true}})
	}
	for _, path := range []string{
		"/api/pool/accounts/codex/exchange", "/api/pool/accounts/claude/add",
		"/api/pool/accounts/antigravity/status", "/api/pool/accounts/zai/login/poll",
		"/api/pool/accounts/grok/add", "/api/pool/accounts/opencode-go/add",
	} {
		tests = append(tests, struct {
			name    string
			method  string
			path    string
			body    string
			allowed map[PrincipalKind]bool
		}{"provider " + path, http.MethodPost, path, `{}`, map[PrincipalKind]bool{PrincipalOperator: true}})
	}
	for _, test := range tests {
		for kind, session := range sessions {
			t.Run(test.name+"/"+string(kind), func(t *testing.T) {
				response := httptest.NewRecorder()
				handler.ServeHTTP(response, authorityRequest(test.method, test.path, session, test.body))
				if test.allowed[kind] && (response.Code == http.StatusUnauthorized || response.Code == http.StatusForbidden) {
					t.Fatalf("allowed %s received %d: %s", kind, response.Code, response.Body.String())
				}
				if !test.allowed[kind] && response.Code != http.StatusForbidden {
					t.Fatalf("denied %s received %d, want 403: %s", kind, response.Code, response.Body.String())
				}
			})
		}
	}
}
