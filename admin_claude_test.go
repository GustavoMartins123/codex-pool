package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"
)

func TestClaudeExchangeIdentifiesAccount(t *testing.T) {
	for _, tc := range []struct {
		name          string
		tokenAccount  any
		profileStatus int
		profileBody   string
		wantStatus    int
		wantProfile   bool
	}{
		{"token account", map[string]string{"uuid": "synthetic-account-uuid"}, 0, "", http.StatusOK, false},
		{"profile fallback", nil, http.StatusOK, `{"account":{"uuid":"synthetic-account-uuid"}}`, http.StatusOK, true},
		{"empty token account", map[string]string{"uuid": ""}, http.StatusOK, `{"account":{"uuid":"synthetic-account-uuid"}}`, http.StatusOK, true},
		{"profile unavailable", nil, http.StatusBadGateway, "<html>Bad Gateway</html>", http.StatusBadGateway, true},
		{"profile missing account", nil, http.StatusOK, `{"organization":{"uuid":"not-an-account"}}`, http.StatusBadGateway, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			originalClient := claudeOAuthHTTPClient
			t.Cleanup(func() { claudeOAuthHTTPClient = originalClient })
			claudeOAuthHTTPClient = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				if r.URL.String() != ClaudeOAuthTokenURL {
					t.Fatalf("unexpected token endpoint: %s", r.URL)
				}
				var request map[string]string
				if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
					t.Fatal(err)
				}
				if request["code"] != "synthetic-code" || request["state"] != "session-state" || request["code_verifier"] != "synthetic-verifier" {
					t.Fatalf("unexpected exchange payload: %+v", request)
				}
				body, _ := json.Marshal(map[string]any{
					"access_token": "sk-ant-oat-synthetic-token", "refresh_token": "synthetic-refresh",
					"expires_in": 3600, "scope": "user:profile user:inference", "account": tc.tokenAccount,
				})
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(body)))}, nil
			})}

			profileCalled := false
			base, _ := url.Parse("https://api.anthropic.com")
			h := &proxyHandler{cfg: &config{poolDir: t.TempDir()}, registry: NewProviderRegistry(nil, NewClaudeProvider(base), nil), transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				profileCalled = true
				if !tc.wantProfile {
					t.Fatal("token account UUID must not require an additional API request")
				}
				if r.Method != http.MethodGet || r.URL.String() != ClaudeOAuthProfileURL {
					t.Fatalf("unexpected profile request: %s %s", r.Method, r.URL)
				}
				if r.Header.Get("Authorization") != "Bearer sk-ant-oat-synthetic-token" {
					t.Fatal("profile request must authenticate with the exchanged access token")
				}
				return &http.Response{StatusCode: tc.profileStatus, Status: http.StatusText(tc.profileStatus), Body: io.NopCloser(strings.NewReader(tc.profileBody))}, nil
			})}
			attachContributionFixture(t, h, "alice")
			claudeOAuthSessions.Lock()
			claudeOAuthSessions.sessions["synthetic-verifier"] = &ClaudeOAuthSession{ActorID: "alice", State: "session-state", CreatedAt: time.Now()}
			claudeOAuthSessions.Unlock()
			t.Cleanup(func() {
				claudeOAuthSessions.Lock()
				delete(claudeOAuthSessions.sessions, "synthetic-verifier")
				claudeOAuthSessions.Unlock()
			})
			r := httptest.NewRequest(http.MethodPost, "/api/pool/accounts/claude/exchange", strings.NewReader(`{"code":"synthetic-code#session-state","verifier":"synthetic-verifier"}`))
			r.Header.Set("Content-Type", "application/json")
			w := httptest.NewRecorder()
			h.handleClaudeExchange(w, contributionRequestActor(r, "alice"))
			if w.Code != tc.wantStatus {
				t.Fatalf("status=%d, want %d: %s", w.Code, tc.wantStatus, w.Body.String())
			}
			if profileCalled != tc.wantProfile {
				t.Fatalf("profile called=%v, want %v", profileCalled, tc.wantProfile)
			}
			if w.Header().Get("Content-Type") != "application/json" || !json.Valid(w.Body.Bytes()) {
				t.Fatalf("response must be JSON, including on identification failure: %s", w.Body.String())
			}
			accounts := h.pool.allAccounts()
			if tc.wantStatus != http.StatusOK {
				if len(accounts) != 0 {
					t.Fatal("failed identification must not save an account")
				}
				return
			}
			if len(accounts) != 1 || accounts[0].AccountUUID != "synthetic-account-uuid" {
				t.Fatalf("expected account UUID to be persisted, got %+v", accounts)
			}
			assertEncryptedCredentialFile(t, accounts[0].File, "sk-ant-oat-synthetic-token")
		})
	}
}

func claudeReloginFixture(t *testing.T) (*proxyHandler, *Account, authoritySession) {
	t.Helper()
	base, _ := url.Parse("https://api.anthropic.com")
	h := &proxyHandler{cfg: &config{poolDir: t.TempDir(), adminToken: "synthetic-admin"}, registry: NewProviderRegistry(nil, NewClaudeProvider(base), nil)}
	attachContributionFixture(t, h, "alice")
	r := contributionRequestActor(httptest.NewRequest(http.MethodPost, "/", nil), "alice")
	_, err := h.saveContribution(r, AccountTypeClaude, "synthetic-account-uuid", map[string]any{
		"account_uuid": "synthetic-account-uuid", "added_at": "2026-01-01T00:00:00Z",
		"disabled": true, "dead": true, "needs_login": true, "auth_error": "session expired",
		"external_id": json.Number("9007199254740993"), "allowed_source_ips": []string{"192.0.2.1"},
		"claudeAiOauth": map[string]any{
			"accessToken": "sk-ant-oat-old", "refreshToken": "old-refresh", "expiresAt": 1,
			"subscriptionType": "max", "rateLimitTier": "default_claude_max_20x",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	account := h.pool.allAccounts()[0]
	account.NeedsLogin, account.AuthError, account.RefreshBlocked = true, "session expired", true
	account.Penalty, account.Totals.TotalInputTokens = 3, 123
	session := addAuthorityPrincipal(t, h.passport, "alice", PrincipalOperator)
	return h, account, session
}

func startClaudeReloginTest(t *testing.T, h *proxyHandler, account *Account, actor authoritySession) map[string]string {
	t.Helper()
	body, _ := json.Marshal(map[string]string{"account_id": account.ID})
	w := httptest.NewRecorder()
	h.ServeHTTP(w, authorityRequest(http.MethodPost, "/admin/claude/relogin", actor, string(body)))
	if w.Code != http.StatusOK {
		t.Fatalf("start relogin: %d %s", w.Code, w.Body.String())
	}
	var result map[string]string
	if err := json.Unmarshal(w.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		claudeOAuthSessions.Lock()
		delete(claudeOAuthSessions.sessions, result["verifier"])
		claudeOAuthSessions.Unlock()
	})
	return result
}

func TestClaudeReloginReplacesCredentialsInPlace(t *testing.T) {
	h, account, actor := claudeReloginFixture(t)
	resourcesBefore, err := h.passport.ownedAccountResources("alice")
	if err != nil {
		t.Fatal(err)
	}
	login := startClaudeReloginTest(t, h, account, actor)
	authorizeURL, _ := url.Parse(login["oauth_url"])
	if login["account_id"] != account.ID || authorizeURL.Query().Get("state") != login["state"] || authorizeURL.Query().Get("code_challenge") == "" {
		t.Fatal("relogin did not return a bound PKCE session")
	}
	claudeOAuthSessions.RLock()
	session := claudeOAuthSessions.sessions[login["verifier"]]
	claudeOAuthSessions.RUnlock()
	if session.ActorID != "alice" || session.ReloginAccountID != account.ID {
		t.Fatalf("session binding: %+v", session)
	}
	originalClient := claudeOAuthHTTPClient
	t.Cleanup(func() { claudeOAuthHTTPClient = originalClient })
	exchanges := 0
	claudeOAuthHTTPClient = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		exchanges++
		var payload map[string]string
		if err := json.NewDecoder(r.Body).Decode(&payload); err != nil {
			t.Fatal(err)
		}
		if payload["code"] != "synthetic-code" || payload["state"] != login["state"] || payload["code_verifier"] != login["verifier"] {
			t.Fatalf("exchange payload: %+v", payload)
		}
		return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"access_token":"sk-ant-oat-new","refresh_token":"new-refresh","expires_in":3600,"scope":"user:profile user:inference","account":{"uuid":"synthetic-account-uuid"}}`))}, nil
	})}
	body, _ := json.Marshal(map[string]string{"code": "synthetic-code#" + login["state"], "verifier": login["verifier"]})
	exchange := func() *httptest.ResponseRecorder {
		w := httptest.NewRecorder()
		h.ServeHTTP(w, authorityRequest(http.MethodPost, "/admin/claude/exchange", actor, string(body)))
		return w
	}
	w := exchange()
	if w.Code != http.StatusOK || !strings.Contains(w.Body.String(), `"replaced":true`) {
		t.Fatalf("exchange: %d %s", w.Code, w.Body.String())
	}
	if accounts := h.pool.allAccounts(); len(accounts) != 1 || accounts[0] != account {
		t.Fatal("relogin replaced the account instead of its credentials")
	}
	if account.AccessToken != "sk-ant-oat-new" || account.RefreshToken != "new-refresh" || !account.ExpiresAt.After(time.Now()) || account.LastRefresh.IsZero() {
		t.Fatal("new credentials were not activated")
	}
	if account.Dead || account.NeedsLogin || account.AuthError != "" || account.RefreshBlocked {
		t.Fatal("relogin left stale authentication failure state")
	}
	if !account.Disabled || account.Penalty != 3 || account.Totals.TotalInputTokens != 123 {
		t.Fatal("relogin lost administrative or runtime state")
	}
	raw, err := readAccountFile(account.File)
	if err != nil {
		t.Fatal(err)
	}
	var saved map[string]json.RawMessage
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{"dead", "needs_login", "auth_error"} {
		if _, exists := saved[key]; exists {
			t.Fatalf("stale %s persisted after relogin", key)
		}
	}
	if string(saved["external_id"]) != "9007199254740993" || string(saved["added_at"]) != `"2026-01-01T00:00:00Z"` || !strings.Contains(string(saved["allowed_source_ips"]), "192.0.2.1") {
		t.Fatal("relogin lost account metadata")
	}
	if !strings.Contains(string(saved["claudeAiOauth"]), `"subscriptionType": "max"`) || !strings.Contains(string(saved["claudeAiOauth"]), "default_claude_max_20x") {
		t.Fatal("relogin lost subscription metadata")
	}
	assertEncryptedCredentialFile(t, account.File, "sk-ant-oat-new")
	if err := h.reloadAccounts(); err != nil {
		t.Fatal(err)
	}
	if account.AccessToken != "sk-ant-oat-new" || account.Dead || account.NeedsLogin || account.AuthError != "" {
		t.Fatal("reloading undid relogin")
	}
	resourcesAfter, err := h.passport.ownedAccountResources("alice")
	if err != nil || !reflect.DeepEqual(resourcesBefore, resourcesAfter) {
		t.Fatal("relogin changed account ownership or controls")
	}
	if replay := exchange(); replay.Code != http.StatusBadRequest || exchanges != 1 {
		t.Fatalf("session was reusable: %d, exchanges=%d", replay.Code, exchanges)
	}
}

func TestClaudeReloginRejectsUnauthorizedAndInvalidStarts(t *testing.T) {
	h, account, actor := claudeReloginFixture(t)
	other := addAuthorityPrincipal(t, h.passport, "bob", PrincipalOperator)
	for _, tc := range []struct {
		name, body string
		actor      authoritySession
		want       int
	}{
		{"invalid JSON", "{", actor, http.StatusBadRequest},
		{"missing ID", `{}`, actor, http.StatusBadRequest},
		{"unknown account", `{"account_id":"unknown"}`, actor, http.StatusNotFound},
		{"another owner", `{"account_id":"` + account.ID + `"}`, other, http.StatusForbidden},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := httptest.NewRecorder()
			h.ServeHTTP(w, authorityRequest(http.MethodPost, "/admin/claude/relogin", tc.actor, tc.body))
			if w.Code != tc.want {
				t.Fatalf("status=%d, want %d: %s", w.Code, tc.want, w.Body.String())
			}
		})
	}
	account.AccessToken, account.RefreshToken = "sk-ant-api-synthetic", ""
	w := httptest.NewRecorder()
	h.ServeHTTP(w, authorityRequest(http.MethodPost, "/admin/claude/relogin", actor, `{"account_id":"`+account.ID+`"}`))
	if w.Code != http.StatusBadRequest {
		t.Fatalf("API key accepted for OAuth relogin: %d", w.Code)
	}
}

func TestClaudeReloginExchangeRejectsInvalidSessionsAndCredentials(t *testing.T) {
	for _, name := range []string{"wrong account", "missing refresh token", "missing access token", "expired session", "another actor", "contribution route", "withdrawn account", "withdrawn during exchange"} {
		t.Run(name, func(t *testing.T) {
			h, account, actor := claudeReloginFixture(t)
			login := startClaudeReloginTest(t, h, account, actor)
			before, err := readAccountFile(account.File)
			if err != nil {
				t.Fatal(err)
			}
			path, want, wantExchanges := "/admin/claude/exchange", http.StatusBadRequest, 0
			token := &ClaudeTokenResponse{AccessToken: "sk-ant-oat-new", RefreshToken: "new-refresh", ExpiresIn: 3600, Account: &ClaudeOAuthTokenAccount{UUID: "synthetic-account-uuid"}}
			withdraw := func() {
				t.Helper()
				resources, err := h.passport.ownedAccountResources("alice")
				if err != nil {
					t.Fatal(err)
				}
				if err := h.passport.withdrawAccount("alice", AccountTypeClaude, account.ID, resources[0].Revision); err != nil {
					t.Fatal(err)
				}
			}
			switch name {
			case "wrong account":
				token.Account.UUID, wantExchanges = "different-account", 1
			case "missing refresh token":
				token.RefreshToken, wantExchanges = "", 1
			case "missing access token":
				token.AccessToken, wantExchanges = "", 1
			case "expired session":
				claudeOAuthSessions.Lock()
				claudeOAuthSessions.sessions[login["verifier"]].CreatedAt = time.Now().Add(-11 * time.Minute)
				claudeOAuthSessions.Unlock()
			case "another actor":
				actor = addAuthorityPrincipal(t, h.passport, "bob", PrincipalOperator)
				want = http.StatusForbidden
			case "contribution route":
				path, want = "/api/pool/accounts/claude/exchange", http.StatusForbidden
			case "withdrawn account":
				withdraw()
				want = http.StatusForbidden
			case "withdrawn during exchange":
				want, wantExchanges = http.StatusForbidden, 1
			}
			originalClient := claudeOAuthHTTPClient
			t.Cleanup(func() { claudeOAuthHTTPClient = originalClient })
			exchanges := 0
			claudeOAuthHTTPClient = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				exchanges++
				if name == "withdrawn during exchange" {
					withdraw()
				}
				data, _ := json.Marshal(token)
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(string(data)))}, nil
			})}
			body, _ := json.Marshal(map[string]string{"code": "synthetic-code#" + login["state"], "verifier": login["verifier"]})
			w := httptest.NewRecorder()
			h.ServeHTTP(w, authorityRequest(http.MethodPost, path, actor, string(body)))
			if w.Code != want || exchanges != wantExchanges {
				t.Fatalf("status=%d, want %d; exchanges=%d, want %d; body=%s", w.Code, want, exchanges, wantExchanges, w.Body.String())
			}
			after, err := readAccountFile(account.File)
			if err != nil || string(before) != string(after) || account.AccessToken != "sk-ant-oat-old" || !account.NeedsLogin {
				t.Fatal("rejected relogin modified existing credentials")
			}
		})
	}
}
