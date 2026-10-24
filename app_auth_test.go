package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/golang-jwt/jwt/v5"
	"go.etcd.io/bbolt"
)

func appAuthHandler(t *testing.T) *proxyHandler {
	t.Helper()
	t.Setenv("POOL_JWT_SECRET", "test-mobile-jwt-secret")
	return newLoginBruteForceHandler(t)
}

func appAuthRequest(h *proxyHandler, method, path, token, body string) *httptest.ResponseRecorder {
	r := httptest.NewRequest(method, path, strings.NewReader(body))
	r.Header.Set("Content-Type", "application/json")
	if token != "" {
		r.Header.Set("Authorization", "Bearer "+token)
	}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	return w
}

func appSignIn(t *testing.T, h *proxyHandler) appLoginResponse {
	t.Helper()
	w := appAuthRequest(h, "POST", "/api/auth/token", "", `{"email":"brute@example.com","password":"correct horse battery"}`)
	if w.Code != http.StatusOK {
		t.Fatalf("app sign in = %d: %s", w.Code, w.Body.String())
	}
	var result appLoginResponse
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func TestAppLoginUsesWebsiteCredentialsWithoutBrowserSession(t *testing.T) {
	h := appAuthHandler(t)
	var sessionsBefore int
	_ = h.passport.db.View(func(tx *bbolt.Tx) error {
		sessionsBefore = tx.Bucket([]byte(bucketPassportSessions)).Stats().KeyN
		return nil
	})
	w := appAuthRequest(h, "POST", "/api/auth/token", "", `{"email":" BRUTE@EXAMPLE.COM ","password":"correct horse battery"}`)
	if w.Code != 200 || len(w.Result().Cookies()) != 0 || w.Header().Get("Cache-Control") != "no-store" {
		t.Fatalf("login status=%d cookies=%d", w.Code, len(w.Result().Cookies()))
	}
	var result appLoginResponse
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil || result.Token == "" || result.ClientID == "" {
		t.Fatalf("invalid app login response: %v", err)
	}
	principal := h.passport.byLogin("brute@example.com")
	client := h.passport.clients[result.ClientID]
	if client == nil || client.PrincipalID != principal.ID || client.Purpose != appClientPurpose || client.DownloadToken != "" {
		t.Fatal("app credential not persisted with its owner/purpose")
	}
	if time.Until(result.ExpiresAt) <= 29*24*time.Hour || time.Until(result.ExpiresAt) > appTokenLifetime {
		t.Fatal("incorrect app token expiry")
	}
	if err := h.passport.db.View(func(tx *bbolt.Tx) error {
		if tx.Bucket([]byte(bucketPassportSessions)).Stats().KeyN != sessionsBefore {
			t.Fatal("app login created a browser session")
		}
		if strings.Contains(string(tx.Bucket([]byte(bucketClientCredentials)).Get([]byte(result.ClientID))), result.Token) {
			t.Fatal("JWT persisted in client record")
		}
		found := false
		_ = tx.Bucket([]byte(bucketPassportAudit)).ForEach(func(_, raw []byte) error {
			var entry AuditEntry
			if json.Unmarshal(raw, &entry) == nil && entry.Action == "app.login" && entry.SubjectID == result.ClientID {
				found = true
			}
			return nil
		})
		if !found {
			t.Fatal("app login audit missing")
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if w := appAuthRequest(h, "GET", "/api/auth/me", result.Token, ""); w.Code != 200 {
		t.Fatalf("app bearer me = %d", w.Code)
	}
	if _, _, _, _, allowed := h.authorizePoolCredentialRequest(httptest.NewRequest("GET", "/v1/models", nil)); allowed {
		t.Fatal("unauthenticated request authorized")
	}
	r := httptest.NewRequest("GET", "/v1/models", nil)
	r.Header.Set("Authorization", "Bearer "+result.Token)
	if _, _, _, _, allowed := h.authorizePoolCredentialRequest(r); allowed {
		t.Fatal("app control token accepted as LLM credential")
	}
	if w := appAuthRequest(h, "GET", "/api/me/clients", result.Token, ""); w.Code != 401 {
		t.Fatal("app token authorized credential administration")
	}
}

func TestAppAndWebsiteShareLoginRateLimit(t *testing.T) {
	h := appAuthHandler(t)
	for i := 0; i < bruteForceMaxAttempts; i++ {
		path := "/api/auth/login"
		if i%2 == 0 {
			path = "/api/auth/token"
		}
		if w := appAuthRequest(h, "POST", path, "", `{"email":"brute@example.com","password":"wrong"}`); w.Code != 401 {
			t.Fatalf("attempt %d = %d", i, w.Code)
		}
	}
	for _, path := range []string{"/api/auth/login", "/api/auth/token"} {
		if w := appAuthRequest(h, "POST", path, "", `{"email":"brute@example.com","password":"correct horse battery"}`); w.Code != 429 {
			t.Fatalf("banned %s = %d", path, w.Code)
		}
	}
}

func TestAppLoginAcceptsWebsiteUsernameAndCapsExpiry(t *testing.T) {
	h := appAuthHandler(t)
	principal := h.passport.byLogin("brute@example.com")
	principal.Username = "mobile-user"
	expires := time.Now().UTC().Add(time.Hour)
	principal.ExpiresAt = &expires
	if err := h.passport.db.Update(func(tx *bbolt.Tx) error { return putJSON(tx.Bucket([]byte(bucketPrincipals)), principal.ID, principal) }); err != nil {
		t.Fatal(err)
	}
	h.passport.mu.Lock()
	h.passport.principals[principal.ID] = principal
	h.passport.mu.Unlock()
	w := appAuthRequest(h, "POST", "/api/auth/token", "", `{"email":" MOBILE-USER ","password":"correct horse battery"}`)
	if w.Code != 200 {
		t.Fatalf("username sign in = %d", w.Code)
	}
	var result appLoginResponse
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	if result.ExpiresAt.After(expires) {
		t.Fatal("app session outlives principal")
	}
}

func TestAppLogoutRevokesOnlyItsCredential(t *testing.T) {
	h := appAuthHandler(t)
	first, second := appSignIn(t, h), appSignIn(t, h)
	if w := appAuthRequest(h, "DELETE", "/api/auth/token", first.Token, ""); w.Code != 204 {
		t.Fatalf("logout = %d", w.Code)
	}
	if w := appAuthRequest(h, "GET", "/api/auth/me", first.Token, ""); w.Code != 401 {
		t.Fatal("revoked app token accepted")
	}
	if w := appAuthRequest(h, "GET", "/api/auth/me", second.Token, ""); w.Code != 200 {
		t.Fatal("logout revoked another app credential")
	}
}

func TestAppInvalidBearerCannotUseValidBrowserCookie(t *testing.T) {
	h := appAuthHandler(t)
	_, browser, _, err := h.passport.login("brute@example.com", "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/auth/me", "/api/me/quotas", "/api/pool/catalog"} {
		r := httptest.NewRequest("GET", path, nil)
		r.AddCookie(&http.Cookie{Name: "pool_session", Value: browser})
		r.Header.Set("Authorization", "Bearer invalid")
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if w.Code != 401 {
			t.Fatalf("invalid bearer with valid cookie %s = %d", path, w.Code)
		}
	}
}

func TestAppTokenValidationRejectsWrongScopeAndExpiry(t *testing.T) {
	h := appAuthHandler(t)
	login := appSignIn(t, h)
	var original appTokenClaims
	if _, err := jwt.ParseWithClaims(login.Token, &original, func(*jwt.Token) (any, error) { return []byte(getPoolJWTSecret()), nil }); err != nil {
		t.Fatal(err)
	}
	for name, mutate := range map[string]func(*appTokenClaims){
		"expired":                func(c *appTokenClaims) { c.ExpiresAt = jwt.NewNumericDate(time.Now().Add(-time.Minute)) },
		"missing expiry":         func(c *appTokenClaims) { c.ExpiresAt = nil },
		"missing issued at":      func(c *appTokenClaims) { c.IssuedAt = nil },
		"LLM issuer":             func(c *appTokenClaims) { c.Issuer = "https://auth.openai.com" },
		"wrong audience":         func(c *appTokenClaims) { c.Audience = jwt.ClaimStrings{"other-client"} },
		"wrong client":           func(c *appTokenClaims) { c.ClientID = "other-client" },
		"wrong password version": func(c *appTokenClaims) { c.PasswordChangedAt = "other" },
	} {
		t.Run(name, func(t *testing.T) {
			claims := original
			mutate(&claims)
			token, err := jwt.NewWithClaims(jwt.SigningMethodHS256, claims).SignedString([]byte(getPoolJWTSecret()))
			if err != nil {
				t.Fatal(err)
			}
			if w := appAuthRequest(h, "GET", "/api/auth/me", token, ""); w.Code != 401 {
				t.Fatalf("invalid token = %d", w.Code)
			}
		})
	}
}

func TestAppPasswordChangeAndSuspensionInvalidateTokens(t *testing.T) {
	for _, change := range []string{"password", "suspend"} {
		t.Run(change, func(t *testing.T) {
			h := appAuthHandler(t)
			login := appSignIn(t, h)
			principal := h.passport.byLogin("brute@example.com")
			var err error
			if change == "password" {
				_, err = h.passport.ChangePrincipalPassword(principal.ID, principal.ID, "replacement strong password")
			} else {
				_, err = h.passport.setPrincipalStatus("operator", principal.ID, PrincipalSuspended)
			}
			if err != nil {
				t.Fatal(err)
			}
			if w := appAuthRequest(h, "GET", "/api/auth/me", login.Token, ""); w.Code != 401 {
				t.Fatal("inactive session accepted")
			}
		})
	}
}

func TestAppIssuanceRevalidatesPasswordAndRollsBackAuditFailure(t *testing.T) {
	h := appAuthHandler(t)
	principal, err := h.passport.verifyLogin("brute@example.com", "correct horse battery")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = h.passport.ChangePrincipalPassword(principal.ID, principal.ID, "replacement strong password"); err != nil {
		t.Fatal(err)
	}
	if _, err = h.passport.createAppToken(principal, getPoolJWTSecret()); err == nil {
		t.Fatal("stale password verification minted token")
	}
	principal, err = h.passport.verifyLogin("brute@example.com", "replacement strong password")
	if err != nil {
		t.Fatal(err)
	}
	before := len(h.passport.clients)
	if err := h.passport.db.Update(func(tx *bbolt.Tx) error { return tx.DeleteBucket([]byte(bucketPassportAudit)) }); err != nil {
		t.Fatal(err)
	}
	if _, err = h.passport.createAppToken(principal, getPoolJWTSecret()); err == nil {
		t.Fatal("issued token without audit persistence")
	}
	if len(h.passport.clients) != before {
		t.Fatal("failed issuance changed live registry")
	}
	_ = h.passport.db.View(func(tx *bbolt.Tx) error {
		if tx.Bucket([]byte(bucketClientCredentials)).Stats().KeyN != before {
			t.Fatal("failed issuance persisted client")
		}
		return nil
	})
}

func TestAppLoginBoundsAndClientLimit(t *testing.T) {
	h := appAuthHandler(t)
	for _, method := range []string{"GET", "PUT", "PATCH"} {
		if w := appAuthRequest(h, method, "/api/auth/token", "", ""); w.Code != 405 {
			t.Fatalf("method %s = %d", method, w.Code)
		}
	}
	if w := appAuthRequest(h, "POST", "/api/auth/token", "", strings.Repeat("x", (32<<10)+1)); w.Code != 400 {
		t.Fatal("oversize login accepted")
	}
	if w := appAuthRequest(h, "POST", "/api/auth/token", "", `{"username":"brute@example.com","password":"correct horse battery"}`); w.Code != 401 {
		t.Fatal("noncanonical username field accepted")
	}
	principal := h.passport.byLogin("brute@example.com")
	for i := 0; i < 20; i++ {
		if _, err := h.passport.createClient(principal.ID, "existing client", nil); err != nil {
			t.Fatal(err)
		}
	}
	if w := appAuthRequest(h, "POST", "/api/auth/token", "", `{"email":"brute@example.com","password":"correct horse battery"}`); w.Code != 409 {
		t.Fatalf("client limit = %d", w.Code)
	}
}

func TestAppBearerQuotasAndCatalogRespectClientPolicy(t *testing.T) {
	t.Setenv("POOL_JWT_SECRET", "test-mobile-jwt-secret")
	h, principalID, _, _, _, _ := memberQuotaFixture(t)
	principal := h.passport.principal(principalID)
	saveQuotaPolicy(t, h, principalID, ClientPolicy{Providers: PolicySelector{Allow: []string{"*"}}})
	login, err := h.passport.createAppToken(principal, getPoolJWTSecret())
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/me/quotas", "/api/pool/catalog"} {
		w := appAuthRequest(h, "GET", path, login.Token, "")
		if w.Code != 200 || strings.Contains(w.Body.String(), "private-") || strings.Contains(w.Body.String(), "never-expose") {
			t.Fatalf("projection %s = %d", path, w.Code)
		}
	}
	clientPolicy := ClientPolicy{Models: PolicySelector{Allow: []string{"gpt-5.6-sol"}}}
	if err := h.passport.saveEditedPolicy("test-operator", principalID, policyEditorRequest{
		Revision: h.passport.principal(principalID).PolicyRevision, Target: "client", ClientID: login.ClientID, Policy: &clientPolicy,
	}); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{"/api/me/quotas", "/api/pool/catalog"} {
		w := appAuthRequest(h, "GET", path, login.Token, "")
		if w.Code != 200 || strings.Contains(w.Body.String(), "claude-opus") {
			t.Fatalf("client policy ignored by %s: %d", path, w.Code)
		}
	}
}
