package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
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
