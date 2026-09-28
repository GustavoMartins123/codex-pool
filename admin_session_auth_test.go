package main

import (
	"net/http"
	"net/http/httptest"
	"testing"
)

func TestAdminRoutesRequireOperatorSessionOrBreakGlassToken(t *testing.T) {
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "test-passport-encryption-key")
	store := testUsageStore(t)
	passport, err := newPassportStore(store.db)
	if err != nil {
		t.Fatal(err)
	}
	operator := addAuthorityPrincipal(t, passport, "operator", PrincipalOperator)
	member := addAuthorityPrincipal(t, passport, "member", PrincipalMember)
	h := &proxyHandler{cfg: &config{adminToken: "break-glass-admin-token"}, passport: passport, pool: newPoolState(nil, false)}

	tests := []struct {
		name    string
		request *http.Request
		want    int
	}{
		{"operator session", authorityRequest(http.MethodGet, "/admin/accounts", operator, ""), http.StatusOK},
		{"member session", authorityRequest(http.MethodGet, "/admin/accounts", member, ""), http.StatusForbidden},
		{"no credentials", httptest.NewRequest(http.MethodGet, "/admin/accounts", nil), http.StatusUnauthorized},
	}
	memberWithToken := authorityRequest(http.MethodGet, "/admin/accounts", member, "")
	memberWithToken.Header.Set("X-Admin-Token", "break-glass-admin-token")
	tests = append(tests, struct {
		name    string
		request *http.Request
		want    int
	}{"break-glass token overrides member cookie", memberWithToken, http.StatusOK})
	breakGlass := httptest.NewRequest(http.MethodGet, "/admin/accounts", nil)
	breakGlass.Header.Set("X-Admin-Token", "break-glass-admin-token")
	tests = append(tests, struct {
		name    string
		request *http.Request
		want    int
	}{"break-glass without session", breakGlass, http.StatusOK})

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			response := httptest.NewRecorder()
			h.ServeHTTP(response, test.request)
			if response.Code != test.want {
				t.Fatalf("status = %d, want %d; body=%s", response.Code, test.want, response.Body.String())
			}
		})
	}

	for _, path := range []string{
		"/admin/reload",
		"/admin/accounts/example/enable",
		"/admin/accounts/example/disable",
		"/admin/accounts/example/resurrect",
		"/admin/accounts/example/refresh",
		"/admin/codex/relogin",
		"/admin/codex/exchange",
		"/admin/antigravity/relogin",
		"/admin/antigravity/exchange",
	} {
		t.Run("member mutation "+path, func(t *testing.T) {
			response := httptest.NewRecorder()
			h.ServeHTTP(response, authorityRequest(http.MethodPost, path, member, "{}"))
			if response.Code != http.StatusForbidden {
				t.Fatalf("status = %d, want 403", response.Code)
			}
		})
	}

	post := authorityRequest(http.MethodPost, "/admin/reload", operator, "")
	post.Header.Del("X-CSRF-Token")
	response := httptest.NewRecorder()
	h.ServeHTTP(response, post)
	if response.Code != http.StatusForbidden {
		t.Fatalf("operator mutation without CSRF status = %d, want 403", response.Code)
	}

	validPost := authorityRequest(http.MethodPost, "/admin/reload", operator, "")
	if !h.checkAdminAuth(httptest.NewRecorder(), validPost) {
		t.Fatal("operator mutation with CSRF was rejected")
	}
	breakGlassPost := httptest.NewRequest(http.MethodPost, "/admin/reload", nil)
	breakGlassPost.Header.Set("X-Admin-Token", "break-glass-admin-token")
	if !h.checkAdminAuth(httptest.NewRecorder(), breakGlassPost) {
		t.Fatal("break-glass mutation without CSRF was rejected")
	}
	memberWithBreakGlass := authorityRequest(http.MethodPost, "/admin/reload", member, "")
	memberWithBreakGlass.Header.Del("X-CSRF-Token")
	memberWithBreakGlass.Header.Set("X-Admin-Token", "break-glass-admin-token")
	if !h.checkAdminAuth(httptest.NewRecorder(), memberWithBreakGlass) {
		t.Fatal("break-glass header was blocked by a member cookie")
	}
	if !h.checkProviderContributionAuth(httptest.NewRecorder(), memberWithBreakGlass) || providerContributionActor(memberWithBreakGlass) != "break-glass" {
		t.Fatal("provider contribution did not honor explicit break-glass authority")
	}
	getWithoutCSRF := authorityRequest(http.MethodGet, "/admin/accounts", operator, "")
	getWithoutCSRF.Header.Del("X-CSRF-Token")
	if !h.checkAdminAuth(httptest.NewRecorder(), getWithoutCSRF) {
		t.Fatal("operator GET without CSRF was rejected")
	}
}
