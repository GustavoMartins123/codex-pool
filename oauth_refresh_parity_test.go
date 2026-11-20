package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

type oauthRefreshParityCase struct {
	Provider     string `json:"provider"`
	Scenario     string `json:"scenario"`
	Status       int    `json:"status"`
	Body         string `json:"body"`
	TransportErr string `json:"transport_err"`
	RefreshToken string `json:"refresh_token"`
	AccountID    string `json:"account_id"`
	Request      string `json:"request"`
	Account      string `json:"account"`
	Err          string `json:"err"`
}

type oauthRefreshParityTransport struct {
	tc      oauthRefreshParityCase
	request string
}

func (rt *oauthRefreshParityTransport) RoundTrip(req *http.Request) (*http.Response, error) {
	body, _ := io.ReadAll(req.Body)
	captured := map[string]any{"method": req.Method, "url": req.URL.String(), "header": req.Header, "body": string(body)}
	encoded, _ := json.Marshal(captured)
	rt.request = string(encoded)
	if rt.tc.TransportErr != "" {
		return nil, errors.New(rt.tc.TransportErr)
	}
	return &http.Response{
		StatusCode: rt.tc.Status,
		Status:     fmt.Sprintf("%d %s", rt.tc.Status, http.StatusText(rt.tc.Status)),
		Header:     http.Header{"Content-Type": {"application/json"}},
		Body:       io.NopCloser(strings.NewReader(rt.tc.Body)),
	}, nil
}

func oauthRefreshParityProvider(t *testing.T, name string) (Authenticator, AccountType) {
	t.Helper()
	refreshBase, _ := url.Parse("https://auth.openai.example.test")
	switch name {
	case "codex":
		return NewCodexProvider(refreshBase, refreshBase, refreshBase), AccountTypeCodex
	case "gemini":
		return NewGeminiProvider(refreshBase, refreshBase), AccountTypeGemini
	case "grok":
		return NewGrokProvider(refreshBase), AccountTypeGrok
	case "antigravity":
		return NewAntigravityProvider(refreshBase, refreshBase), AccountTypeAntigravity
	}
	t.Fatalf("unknown provider %q", name)
	return nil, ""
}

// normalizeOAuthRefreshTime keeps times that do not depend on the clock and
// replaces the ones derived from it with an offset from the call window.
func normalizeOAuthRefreshTime(value time.Time, start, end time.Time) string {
	if value.IsZero() {
		return ""
	}
	for _, offset := range []time.Duration{0, time.Hour, -5 * time.Second} {
		if !value.Before(start.Add(offset-time.Second)) && !value.After(end.Add(offset+time.Second)) {
			return fmt.Sprintf("<now+%s>", offset)
		}
	}
	return value.UTC().Format(time.RFC3339)
}

func runOAuthRefreshParity(t *testing.T, tc oauthRefreshParityCase) oauthRefreshParityCase {
	t.Helper()
	t.Setenv("GEMINI_OAUTH_CLIENT_ID", "gemini-client-fixture")
	t.Setenv("GEMINI_OAUTH_CLIENT_SECRET", "gemini-secret-fixture")
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_ID", "antigravity-client-fixture")
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_SECRET", "antigravity-secret-fixture")
	provider, accountType := oauthRefreshParityProvider(t, tc.Provider)
	file := filepath.Join(t.TempDir(), "account.json")
	if err := os.WriteFile(file, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	acc := &Account{ID: "account", Type: accountType, File: file, AccessToken: "old-access", RefreshToken: tc.RefreshToken, AccountID: tc.AccountID, PlanType: "old-plan", Dead: true, NeedsLogin: true, AuthError: "old-error"}
	transport := &oauthRefreshParityTransport{tc: tc}
	start := time.Now()
	err := provider.RefreshToken(t.Context(), acc, transport)
	end := time.Now()
	got := tc
	got.Request = transport.request
	got.Err = ""
	if err != nil {
		got.Err = strings.ReplaceAll(err.Error(), file, "<file>")
	}
	state := map[string]any{
		"access_token": acc.AccessToken, "refresh_token": acc.RefreshToken, "id_token": acc.IDToken,
		"account_id": acc.AccountID, "chatgpt_account_id": acc.IDTokenChatGPTAccountID, "plan_type": acc.PlanType,
		"expires_at": normalizeOAuthRefreshTime(acc.ExpiresAt, start, end), "last_refresh": normalizeOAuthRefreshTime(acc.LastRefresh, start, end),
		"dead": acc.Dead, "needs_login": acc.NeedsLogin, "auth_error": acc.AuthError,
	}
	encoded, _ := json.Marshal(state)
	got.Account = string(encoded)
	return got
}

func TestOAuthRefreshParity(t *testing.T) {
	data, err := os.ReadFile("testdata/oauth_refresh_parity.json")
	if err != nil {
		t.Fatal(err)
	}
	var cases []oauthRefreshParityCase
	if err := json.Unmarshal(data, &cases); err != nil {
		t.Fatal(err)
	}
	for i, tc := range cases {
		t.Run(fmt.Sprintf("%s/%s/%d", tc.Provider, tc.Scenario, i), func(t *testing.T) {
			if got := runOAuthRefreshParity(t, tc); got != tc {
				t.Fatalf("got  %+v\nwant %+v", got, tc)
			}
		})
	}
}
