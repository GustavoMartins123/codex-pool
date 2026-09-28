package main

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"go.etcd.io/bbolt"
)

func testUsageStore(t *testing.T) *usageStore {
	t.Helper()
	s, err := newUsageStore(filepath.Join(t.TempDir(), "proxy.db"), 30)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

func TestPassportMigratesLegacyUserAndClient(t *testing.T) {
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "test-passport-encryption-key")
	s := testUsageStore(t)
	legacy := &PoolUserStore{users: map[string]*PoolUser{}, byTok: map[string]*PoolUser{}}
	u := &PoolUser{ID: "0123456789abcdef", Token: "download-old", Email: "friend@pool.local", PlanType: "pro", CreatedAt: time.Now()}
	legacy.users[u.ID] = u
	legacy.byTok[u.Token] = u
	p, err := newPassportStore(s.db, legacy)
	if err != nil {
		t.Fatal(err)
	}
	if got := p.principal(u.ID); got == nil || got.Note != "legacy: friend@pool.local" {
		t.Fatalf("principal=%+v", got)
	}
	p.mu.RLock()
	c := p.clients["legacy-"+u.ID]
	p.mu.RUnlock()
	if c == nil || c.Label != "LEGACY DEFAULT" {
		t.Fatalf("client=%+v", c)
	}
	token, err := p.clientDownloadToken(c)
	if err != nil {
		t.Fatal(err)
	}
	if token != u.Token {
		t.Fatalf("download token = %q, want %q", token, u.Token)
	}
}

func TestMemberOnboardingAndRecoveryLinksAreSingleUse(t *testing.T) {
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "test-passport-encryption-key")
	store := testUsageStore(t)
	passport, err := newPassportStore(store.db, nil)
	if err != nil {
		t.Fatal(err)
	}

	onboarding, err := passport.createMemberLink("operator", "member@example.com", "Member", "onboard")
	if err != nil {
		t.Fatal(err)
	}
	member, oldSession, _, err := passport.redeemMemberLink(onboarding.Token, "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if member.Kind != PrincipalMember || member.PasswordHash == "" {
		t.Fatalf("member=%+v", member)
	}
	if _, _, _, err := passport.redeemMemberLink(onboarding.Token, "another correct password"); err == nil {
		t.Fatal("onboarding link was reusable")
	}

	recovery, err := passport.createMemberLink("operator", member.Email, "", "recover")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := passport.redeemMemberLink(recovery.Token, "replacement password"); err != nil {
		t.Fatal(err)
	}
	request, _ := http.NewRequest(http.MethodGet, "/api/auth/me", nil)
	request.AddCookie(&http.Cookie{Name: "pool_session", Value: oldSession})
	if principal, _ := passport.authenticate(request); principal != nil {
		t.Fatal("recovery left a prior browser session active")
	}
	if _, _, _, err := passport.login(member.Email, "replacement password"); err != nil {
		t.Fatalf("replacement password rejected: %v", err)
	}
}

func TestLegacySignupClaimsExistingPrincipal(t *testing.T) {
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "test-passport-encryption-key")
	store := testUsageStore(t)
	legacy := &PoolUserStore{users: map[string]*PoolUser{}, byTok: map[string]*PoolUser{}}
	user := &PoolUser{ID: "legacy-person", Token: "legacy-download-token", Email: "legacy@pool.local", PlanType: "pro", CreatedAt: time.Now()}
	legacy.users[user.ID] = user
	legacy.byTok[user.Token] = user
	passport, err := newPassportStore(store.db, legacy)
	if err != nil {
		t.Fatal(err)
	}
	// A legacy code must not create an operator on a fresh deployment.
	if passport.hasOperator() {
		t.Fatal("legacy signup created an operator")
	}
}

func TestBootstrapOperatorClaimsLegacyCredential(t *testing.T) {
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "test-passport-encryption-key")
	t.Setenv("POOL_JWT_SECRET", "test-jwt-secret-for-bootstrap")
	store := testUsageStore(t)
	legacy := &PoolUserStore{users: map[string]*PoolUser{}, byTok: map[string]*PoolUser{}}
	user := &PoolUser{ID: "operator-person", Token: "operator-download-token", Email: "operator@pool.local", PlanType: "pro", CreatedAt: time.Now()}
	legacy.users[user.ID] = user
	legacy.byTok[user.Token] = user
	passport, err := newPassportStore(store.db, legacy)
	if err != nil {
		t.Fatal(err)
	}
	credential, err := generateClaudeAuth(getPoolJWTSecret(), user)
	if err != nil {
		t.Fatal(err)
	}
	principal, err := passport.bootstrapOperator("operator", "", "Nicole", "NicoleLong2803!", credential.AccessToken)
	if err != nil {
		t.Fatal(err)
	}
	if principal.ID != user.ID || principal.Kind != PrincipalOperator || principal.Username != "operator" {
		t.Fatalf("operator=%+v", principal)
	}
	if _, err := passport.bootstrapOperator("another", "", "", "another-long-password", ""); err == nil {
		t.Fatal("second operator bootstrap succeeded")
	}
}

func TestPasswordRoundTrip(t *testing.T) {
	h, err := hashPassword("correct horse battery staple")
	if err != nil {
		t.Fatal(err)
	}
	if !verifyPassword(h, "correct horse battery staple") {
		t.Fatal("valid password rejected")
	}
	if verifyPassword(h, "wrong") {
		t.Fatal("wrong password accepted")
	}
}

func TestLoginRejectsSuspendedPrincipal(t *testing.T) {
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "test-passport-encryption-key")
	store := testUsageStore(t)
	passport, err := newPassportStore(store.db, nil)
	if err != nil {
		t.Fatal(err)
	}
	onboarding, err := passport.createMemberLink("operator", "suspended@example.com", "S", "onboard")
	if err != nil {
		t.Fatal(err)
	}
	member, _, _, err := passport.redeemMemberLink(onboarding.Token, "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := passport.setPrincipalStatus("operator", member.ID, PrincipalSuspended); err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := passport.login(member.Email, "correct horse battery"); err == nil {
		t.Fatal("suspended principal signed in")
	}
	if _, _, _, err := passport.login(member.ID, "correct horse battery"); err == nil {
		t.Fatal("suspended principal signed in by ID login")
	}
	if _, _, _, err := passport.login(member.Email, "wrong-password"); err == nil {
		t.Fatal("wrong password accepted")
	}
}

func newLoginBruteForceHandler(t *testing.T) *proxyHandler {
	t.Helper()
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "test-passport-encryption-key")
	store := testUsageStore(t)
	passport, err := newPassportStore(store.db, nil)
	if err != nil {
		t.Fatal(err)
	}
	onboarding, err := passport.createMemberLink("operator", "brute@example.com", "B", "onboard")
	if err != nil {
		t.Fatal(err)
	}
	if _, _, _, err := passport.redeemMemberLink(onboarding.Token, "correct horse battery"); err != nil {
		t.Fatal(err)
	}
	tracker := newBruteForceTracker()
	t.Cleanup(tracker.stop)
	return &proxyHandler{cfg: &config{}, passport: passport, bruteForce: tracker}
}

func loginRequest(body string) *http.Request {
	request := httptest.NewRequest(http.MethodPost, "/api/auth/login", strings.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	return request
}

func TestLoginRateLimitsRepeatedFailures(t *testing.T) {
	h := newLoginBruteForceHandler(t)
	wrong := `{"email":"brute@example.com","password":"wrong password"}`
	for i := 0; i < bruteForceMaxAttempts; i++ {
		recorder := httptest.NewRecorder()
		h.handlePassportLogin(recorder, loginRequest(wrong))
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d status = %d", i, recorder.Code)
		}
	}
	banned := httptest.NewRecorder()
	h.handlePassportLogin(banned, loginRequest(`{"email":"brute@example.com","password":"correct horse battery"}`))
	if banned.Code != http.StatusTooManyRequests {
		t.Fatalf("valid login after repeated failures status = %d, want 429", banned.Code)
	}
}

func TestLoginRateLimitClearsOnSuccess(t *testing.T) {
	h := newLoginBruteForceHandler(t)
	for i := 0; i < bruteForceMaxAttempts-1; i++ {
		recorder := httptest.NewRecorder()
		h.handlePassportLogin(recorder, loginRequest(`{"email":"brute@example.com","password":"wrong password"}`))
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("attempt %d status = %d", i, recorder.Code)
		}
	}
	ok := httptest.NewRecorder()
	h.handlePassportLogin(ok, loginRequest(`{"email":"brute@example.com","password":"correct horse battery"}`))
	if ok.Code != http.StatusOK {
		t.Fatalf("valid login status = %d body=%s", ok.Code, ok.Body.String())
	}
	for i := 0; i < bruteForceMaxAttempts-1; i++ {
		recorder := httptest.NewRecorder()
		h.handlePassportLogin(recorder, loginRequest(`{"email":"brute@example.com","password":"wrong password"}`))
		if recorder.Code != http.StatusUnauthorized {
			t.Fatalf("post-success attempt %d status = %d", i, recorder.Code)
		}
	}
	final := httptest.NewRecorder()
	h.handlePassportLogin(final, loginRequest(`{"email":"brute@example.com","password":"correct horse battery"}`))
	if final.Code != http.StatusOK {
		t.Fatalf("login after counter reset status = %d", final.Code)
	}
}

func TestClientCredentialLimitAndLabel(t *testing.T) {
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "test-passport-encryption-key")
	s := testUsageStore(t)
	p, err := newPassportStore(s.db, nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = p.createClient("p1", "", nil); err == nil {
		t.Fatal("empty label accepted")
	}
	for i := 0; i < 20; i++ {
		if _, err = p.createClient("p1", "machine", nil); err != nil {
			t.Fatal(err)
		}
	}
	if _, err = p.createClient("p1", "too many", nil); err == nil {
		t.Fatal("limit not enforced")
	}
}

func newPassportWithTwoMembers(t *testing.T) (*PassportStore, *Principal, *Principal) {
	t.Helper()
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "test-passport-encryption-key")
	store := testUsageStore(t)
	passport, err := newPassportStore(store.db, nil)
	if err != nil {
		t.Fatal(err)
	}
	var members []*Principal
	for _, name := range []string{"alpha@example.com", "beta@example.com"} {
		onboarding, err := passport.createMemberLink("operator", name, name, "onboard")
		if err != nil {
			t.Fatal(err)
		}
		if _, _, _, err := passport.redeemMemberLink(onboarding.Token, "correct horse battery"); err != nil {
			t.Fatal(err)
		}
		members = append(members, passport.byEmail(name))
	}
	return passport, members[0], members[1]
}

func TestGuestPassesAreScopedToOwner(t *testing.T) {
	passport, alpha, beta := newPassportWithTwoMembers(t)
	guest, _, _, _, err := passport.createGuest(alpha.ID, "Alpha friend", "Al", nil)
	if err != nil {
		t.Fatal(err)
	}

	betaPasses, err := passport.listPasses(beta)
	if err != nil {
		t.Fatal(err)
	}
	for _, view := range betaPasses {
		if view.ID == guest.ID {
			t.Fatal("member saw another member's pass link")
		}
	}
	alphaPasses, err := passport.listPasses(alpha)
	if err != nil {
		t.Fatal(err)
	}
	if len(alphaPasses) != 1 || alphaPasses[0].ID != guest.ID {
		t.Fatalf("owner pass view = %+v", alphaPasses)
	}
	if alphaPasses[0].Link == "" || alphaPasses[0].Link == "/join#" {
		t.Fatalf("owner link missing: %+v", alphaPasses[0])
	}

	future := time.Now().Add(time.Hour)
	if _, err := passport.updateGuestPass(beta, guest.ID, "hijacked", "H", &future); err == nil {
		t.Fatal("another member updated the pass")
	}
	if _, err := passport.setGuestPassStatus(beta, guest.ID, false); err == nil {
		t.Fatal("another member revoked the pass")
	}
	if _, err := passport.rotateGuestLink(beta, guest.ID); err == nil {
		t.Fatal("another member rotated the pass link")
	}
	if _, err := passport.updateGuestPass(alpha, guest.ID, "renamed", "Al", &future); err != nil {
		t.Fatalf("owner update failed: %v", err)
	}
}

func TestJoinRateLimitsRepeatedFailures(t *testing.T) {
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "test-passport-encryption-key")
	t.Setenv("POOL_JWT_SECRET", "test-jwt-secret-join")
	store := testUsageStore(t)
	passport, err := newPassportStore(store.db, nil)
	if err != nil {
		t.Fatal(err)
	}
	_, _, _, token, err := passport.createGuest("operator", "join guest", "J", nil)
	if err != nil {
		t.Fatal(err)
	}
	tracker := newBruteForceTracker()
	t.Cleanup(tracker.stop)
	h := &proxyHandler{cfg: &config{}, passport: passport, bruteForce: tracker, metrics: newMetrics()}

	body := func(token string) *http.Request {
		request := httptest.NewRequest(http.MethodPost, "/api/auth/join", strings.NewReader(`{"token":"`+token+`"}`))
		request.Header.Set("Content-Type", "application/json")
		return request
	}
	for i := 0; i < bruteForceMaxAttempts; i++ {
		recorder := httptest.NewRecorder()
		h.handleJoin(recorder, body(fmt.Sprintf("wrong-token-%d", i)))
		if recorder.Code != http.StatusNotFound {
			t.Fatalf("attempt %d status = %d", i, recorder.Code)
		}
	}
	banned := httptest.NewRecorder()
	h.handleJoin(banned, body(token))
	if banned.Code != http.StatusTooManyRequests {
		t.Fatalf("valid join after failures status = %d, want 429", banned.Code)
	}
}

func TestRedeemJoinRejectsExpiredPrincipal(t *testing.T) {
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "test-passport-encryption-key")
	store := testUsageStore(t)
	passport, err := newPassportStore(store.db, nil)
	if err != nil {
		t.Fatal(err)
	}
	past := time.Now().Add(-10 * time.Minute)
	guest, link, _, token, err := passport.createGuest("operator", "expired guest", "Exp", nil)
	if err != nil {
		t.Fatal(err)
	}
	guest.ExpiresAt = &past
	if err := passport.db.Update(func(tx *bbolt.Tx) error {
		return putJSON(tx.Bucket([]byte(bucketPrincipals)), guest.ID, guest)
	}); err != nil {
		t.Fatal(err)
	}
	passport.mu.Lock()
	passport.principals[guest.ID] = guest
	passport.mu.Unlock()

	if _, _, _, err := passport.redeemJoin(token); err == nil {
		t.Fatal("redeemJoin should fail when principal is expired")
	}
	_ = link
}

func TestJoinSwitchDoesNotPromptForRevokedOrExpiredLink(t *testing.T) {
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "test-passport-encryption-key")
	t.Setenv("POOL_JWT_SECRET", "test-jwt-secret-join-switch")
	store := testUsageStore(t)
	passport, err := newPassportStore(store.db, nil)
	if err != nil {
		t.Fatal(err)
	}

	operator := insertTestPrincipal(t, passport, "operator-1", PrincipalOperator, "operator", "operator@pool.local")
	memberSession, memberCsrf, err := passport.createSession(operator.ID)
	if err != nil {
		t.Fatal(err)
	}

	guestRevoked, _, _, tokenRevoked, err := passport.createGuest(operator.ID, "Revoked Guest", "Rev", nil)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := passport.setGuestPassStatus(operator, guestRevoked.ID, false); err != nil {
		t.Fatal(err)
	}

	past := time.Now().Add(-time.Hour)
	_, _, _, tokenExpired, err := passport.createGuest(operator.ID, "Expired Guest", "Exp", &past)
	if err != nil {
		t.Fatal(err)
	}

	h := &proxyHandler{cfg: &config{}, passport: passport, metrics: newMetrics()}

	body := func(token string) *http.Request {
		request := httptest.NewRequest(http.MethodPost, "/api/auth/join", strings.NewReader(`{"token":"`+token+`"}`))
		request.Header.Set("Content-Type", "application/json")
		request.AddCookie(&http.Cookie{Name: "pool_session", Value: memberSession})
		request.AddCookie(&http.Cookie{Name: "pool_csrf", Value: memberCsrf})
		request.Header.Set("X-CSRF-Token", memberCsrf)
		return request
	}

	recRevoked := httptest.NewRecorder()
	h.handleJoin(recRevoked, body(tokenRevoked))
	if recRevoked.Code != http.StatusNotFound {
		t.Fatalf("revoked pass join status = %d, want 404: %s", recRevoked.Code, recRevoked.Body.String())
	}
	if strings.Contains(recRevoked.Body.String(), "switch_required") {
		t.Fatalf("revoked pass should not prompt switch_required, body=%s", recRevoked.Body.String())
	}

	recExpired := httptest.NewRecorder()
	h.handleJoin(recExpired, body(tokenExpired))
	if recExpired.Code != http.StatusNotFound {
		t.Fatalf("expired pass join status = %d, want 404: %s", recExpired.Code, recExpired.Body.String())
	}
	if strings.Contains(recExpired.Body.String(), "switch_required") {
		t.Fatalf("expired pass should not prompt switch_required, body=%s", recExpired.Body.String())
	}
}

func TestCredentialCutoffInvalidatesEveryEnvelope(t *testing.T) {
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "test-passport-encryption-key")
	s := testUsageStore(t)
	p, err := newPassportStore(s.db, nil)
	if err != nil {
		t.Fatal(err)
	}
	principal, _, client, _, err := p.createGuest("operator", "Dave from climbing", "Dave", nil)
	if err != nil {
		t.Fatal(err)
	}
	identity := principal.ID + "-c-" + client.ID
	oldUser := &PoolUser{ID: identity, Email: "dave@pool.local", PlanType: "pro", CreatedAt: time.Now()}
	oldCodex, err := generateCodexAuth("test-jwt-secret", oldUser)
	if err != nil {
		t.Fatal(err)
	}
	oldGemini, err := generateGeminiAuth("test-jwt-secret", oldUser)
	if err != nil {
		t.Fatal(err)
	}
	oldGeminiKey := generateGeminiAPIKey("test-jwt-secret", oldUser)
	oldClaude, err := generateClaudeAuth("test-jwt-secret", oldUser)
	if err != nil {
		t.Fatal(err)
	}
	oldDownload := client.DownloadToken

	if _, err = p.setPrincipalStatus("operator", principal.ID, PrincipalSuspended); err != nil {
		t.Fatal(err)
	}
	if _, err = p.setPrincipalStatus("operator", principal.ID, PrincipalActive); err != nil {
		t.Fatal(err)
	}

	assertDenied := func(name, parsedIdentity string, issuedAt time.Time, parsed bool) {
		t.Helper()
		if !parsed {
			t.Fatalf("%s credential did not parse", name)
		}
		if _, _, ok := p.authorizeIssuedCredential(parsedIdentity, issuedAt); ok {
			t.Fatalf("%s credential issued before cutoff was accepted", name)
		}
	}
	id, at, ok := parsePoolUserToken("test-jwt-secret", "Bearer "+oldCodex.Tokens.AccessToken)
	assertDenied("codex", id, at, ok)
	id, at, ok = parseGeminiOAuthPoolToken("test-jwt-secret", oldGemini.AccessToken)
	assertDenied("gemini oauth", id, at, ok)
	id, at, ok = parsePoolGeminiAPIKey("test-jwt-secret", oldGeminiKey)
	assertDenied("gemini api key", id, at, ok)
	id, at, ok = parseClaudePoolCredential("test-jwt-secret", oldClaude.AccessToken)
	assertDenied("claude", id, at, ok)
	if p.clientByDownloadToken(oldDownload) != nil {
		t.Fatal("old setup token survived principal suspension")
	}

	pr, activeClient, ok := p.credentialState(identity)
	if !ok {
		t.Fatal("restored credential is not active")
	}
	newUser := &PoolUser{ID: identity, Email: "dave@pool.local", PlanType: "pro", CreatedAt: time.Now(), credentialIssuedAt: pr.CredentialsValidAfter}
	if activeClient.ValidAfter.After(newUser.credentialIssuedAt) {
		newUser.credentialIssuedAt = activeClient.ValidAfter
	}
	newCodex, err := generateCodexAuth("test-jwt-secret", newUser)
	if err != nil {
		t.Fatal(err)
	}
	id, at, ok = parsePoolUserToken("test-jwt-secret", "Bearer "+newCodex.Tokens.AccessToken)
	if !ok {
		t.Fatal("fresh credential did not parse")
	}
	if _, _, allowed := p.authorizeIssuedCredential(id, at); !allowed {
		t.Fatal("fresh credential issued at cutoff was rejected")
	}
}

func TestSignedAndLegacyRefreshCutoffs(t *testing.T) {
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "test-passport-encryption-key")
	s := testUsageStore(t)
	p, err := newPassportStore(s.db, nil)
	if err != nil {
		t.Fatal(err)
	}
	principal, _, client, _, err := p.createGuest("operator", "Taylor", "Taylor", nil)
	if err != nil {
		t.Fatal(err)
	}
	identity := principal.ID + "-c-" + client.ID
	legacy := "poolrt_" + identity + "_legacy"
	parsedIdentity, _, signed, ok := parsePoolRefreshToken("test-jwt-secret", legacy)
	if !ok || signed || parsedIdentity != identity {
		t.Fatal("legacy refresh token did not parse")
	}
	if _, _, allowed := p.authorizeLegacyRefresh(identity); !allowed {
		t.Fatal("legacy refresh rejected before first cutoff")
	}
	old := generatePoolRefreshToken("test-jwt-secret", identity, time.Now().UTC())
	if _, err = p.setPrincipalStatus("operator", principal.ID, PrincipalSuspended); err != nil {
		t.Fatal(err)
	}
	if _, err = p.setPrincipalStatus("operator", principal.ID, PrincipalActive); err != nil {
		t.Fatal(err)
	}
	if _, _, allowed := p.authorizeLegacyRefresh(identity); allowed {
		t.Fatal("legacy refresh survived first cutoff")
	}
	parsedIdentity, issuedAt, signed, ok := parsePoolRefreshToken("test-jwt-secret", old)
	if !ok || !signed {
		t.Fatal("signed refresh token did not parse")
	}
	if _, _, allowed := p.authorizeIssuedCredential(parsedIdentity, issuedAt); allowed {
		t.Fatal("signed refresh survived cutoff")
	}
}

func TestDuckAnalyticsOutboxDrain(t *testing.T) {
	s := testUsageStore(t)
	d, err := newDuckAnalytics(filepath.Join(t.TempDir(), "usage.duckdb"), s.db)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = d.Close() })
	ru := RequestUsage{Timestamp: time.Now(), AccountID: "a", AccountType: AccountTypeCodex, UserID: "p1", ClientCredentialID: "mac", ProxyRequestID: "req-1", InputTokens: 10, OutputTokens: 5, BillableTokens: 15, Model: "gpt-test"}
	if err = s.recordWithCost(ru, 0.25); err != nil {
		t.Fatal(err)
	}
	d.Notify()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		rows, err := d.UserHourly(context.Background(), "p1", time.Now().Add(-time.Hour))
		if err == nil && len(rows) == 1 {
			if rows[0].ClientCredentialID != "mac" || rows[0].BillableTokens != 15 {
				t.Fatalf("row=%+v", rows[0])
			}
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatal("fact not drained")
}

func TestConsoleUsageHoursFollowsQuery(t *testing.T) {
	request, err := http.NewRequest(http.MethodGet, "/api/console/principals/member/usage?hours=24", nil)
	if err != nil {
		t.Fatal(err)
	}

	if got := consoleUsageHours(request); got != 24 {
		t.Fatalf("hours = %d, want 24", got)
	}
}
