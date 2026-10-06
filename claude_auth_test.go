package main

import (
	"encoding/json"
	"io"
	"net/http"
	"net/url"
	"strings"
	"testing"
)

func TestClaudeAuthorizeUsesExpandedScopes(t *testing.T) {
	t.Parallel()

	rawURL, session, err := ClaudeAuthorize("acct")
	if err != nil {
		t.Fatalf("ClaudeAuthorize: %v", err)
	}
	if session == nil || session.PKCE == nil {
		t.Fatal("expected oauth session with pkce")
	}

	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatalf("parse auth url: %v", err)
	}
	if got := u.Query().Get("scope"); got != ClaudeOAuthAllScopes {
		t.Fatalf("scope = %q, want %q", got, ClaudeOAuthAllScopes)
	}
	if got := u.Query().Get("state"); got != session.State {
		t.Fatalf("state = %q, want %q", got, session.State)
	}
	if got := u.Query().Get("code_challenge"); got != session.PKCE.Challenge {
		t.Fatalf("code_challenge = %q, want %q", got, session.PKCE.Challenge)
	}
}

func TestClaudeExchangeAuthorizationCodeFormats(t *testing.T) {
	originalClient := claudeOAuthHTTPClient
	t.Cleanup(func() { claudeOAuthHTTPClient = originalClient })

	for _, code := range []string{"synthetic-code#session-state", "synthetic-code"} {
		t.Run(code, func(t *testing.T) {
			called := false
			claudeOAuthHTTPClient = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
				called = true
				if r.Method != http.MethodPost || r.URL.String() != ClaudeOAuthTokenURL {
					t.Fatalf("unexpected token request: %s %s", r.Method, r.URL)
				}
				var body map[string]string
				if err := json.NewDecoder(r.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				for key, want := range map[string]string{
					"code": "synthetic-code", "state": "session-state", "code_verifier": "synthetic-verifier",
					"grant_type": "authorization_code", "client_id": ClaudeOAuthClientID, "redirect_uri": ClaudeOAuthRedirectURI,
				} {
					if body[key] != want {
						t.Errorf("%s = %q, want %q", key, body[key], want)
					}
				}
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(`{"access_token":"synthetic-token","refresh_token":"synthetic-refresh","expires_in":3600}`))}, nil
			})}
			tokens, err := ClaudeExchange(code, "synthetic-verifier", "session-state")
			if err != nil {
				t.Fatalf("ClaudeExchange: %v", err)
			}
			if !called || tokens.AccessToken != "synthetic-token" || tokens.RefreshToken != "synthetic-refresh" {
				t.Fatalf("expected token exchange, got %+v (called=%v)", tokens, called)
			}
		})
	}
}

func TestClaudeExchangeRejectsInvalidStateBeforeRequest(t *testing.T) {
	originalClient := claudeOAuthHTTPClient
	t.Cleanup(func() { claudeOAuthHTTPClient = originalClient })
	claudeOAuthHTTPClient = &http.Client{Transport: roundTripperFunc(func(r *http.Request) (*http.Response, error) {
		t.Fatal("invalid state must not reach the token endpoint")
		return nil, nil
	})}

	for _, tc := range []struct{ code, state string }{
		{"synthetic-code#another-state", "session-state"},
		{"synthetic-code#", "session-state"},
		{"synthetic-code", ""},
	} {
		if _, err := ClaudeExchange(tc.code, "synthetic-verifier", tc.state); err == nil {
			t.Errorf("expected state error for code %q and state %q", tc.code, tc.state)
		}
	}
}
