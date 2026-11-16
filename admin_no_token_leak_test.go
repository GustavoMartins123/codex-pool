package main

import (
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

// CP-02 requires that upstream credentials never reach the browser after an
// account is registered. This regression test feeds marker secrets into real
// accounts and asserts none of the admin listing surfaces serialize them.
func TestAdminSurfacesNeverLeakUpstreamCredentials(t *testing.T) {
	t.Setenv("POOL_JWT_SECRET", "test-secret")
	upstream := newFakeCodexUpstream(t)
	upURL, _ := url.Parse(upstream.server.URL)

	const (
		accessMarker  = "LEAKTEST-access-9d1f"
		refreshMarker = "LEAKTEST-refresh-7c2e"
		apiKeyMarker  = "LEAKTEST-apikey-3a8b"
	)

	codex := &Account{ID: "codex-leak", Type: AccountTypeCodex, AccessToken: accessMarker, RefreshToken: refreshMarker, AccountID: "acct-cx", PlanType: "pro", File: "pool/codex/x.json"}
	claude := &Account{ID: "claude-leak", Type: AccountTypeClaude, AccessToken: accessMarker, RefreshToken: refreshMarker, PlanType: "max", ExpiresAt: time.Now().Add(time.Hour)}
	apiKeyAcc := &Account{ID: "kimi-leak", Type: AccountTypeKimi, AccessToken: apiKeyMarker, PlanType: "pro"}

	fx := newCodexProxyFixture(t, upURL, []*Account{codex, claude, apiKeyAcc})
	fx.handler.cfg.adminToken = "admin-token-test"
	fx.handler.store = nil

	surfaces := []string{
		"/admin/accounts",
		"/admin/codex/",
		"/admin/claude/",
		"/admin/origins",
		"/admin/tokens",
		"/status",
	}
	for _, path := range surfaces {
		t.Run(path, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, fx.server.URL+path, nil)
			req.Header.Set("X-Admin-Token", "admin-token-test")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			for _, marker := range []string{accessMarker, refreshMarker, apiKeyMarker} {
				if strings.Contains(string(body), marker) {
					t.Fatalf("%s leaked upstream credential marker %q (status %d)", path, marker, resp.StatusCode)
				}
			}
		})
	}
}

// The on-demand verify probe echoes upstream error bodies into its detail
// field. A forbidden response that embeds the caller's own tokens must never
// carry them back to the operator's browser.
func TestAntigravityVerifySurfacesNeverLeakUpstreamCredentials(t *testing.T) {
	const (
		accessMarker  = "LEAKTEST-ag-access-5b7d"
		refreshMarker = "LEAKTEST-ag-refresh-2e9a"
	)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusForbidden)
		io.WriteString(w, antigravityVerifyFixture)
	}))
	t.Cleanup(upstream.Close)
	agURL, _ := url.Parse(upstream.URL)
	base, _ := url.Parse("https://unused.example.test")

	account := &Account{
		Type:            AccountTypeAntigravity,
		ID:              "ag-leak",
		File:            filepath.Join(t.TempDir(), "ag-leak.json"),
		AccessToken:     accessMarker,
		RefreshToken:    refreshMarker,
		ProjectID:       "project",
		ExpiresAt:       time.Now().Add(time.Hour),
		ModelRateLimits: map[string]time.Time{},
	}
	h := &proxyHandler{
		cfg:       &config{adminToken: "admin-token-test"},
		pool:      newPoolState([]*Account{account}, false),
		registry:  NewProviderRegistry(NewCodexProvider(base, base, base), NewClaudeProvider(base), NewGeminiProvider(base, base), NewAntigravityProvider(agURL, agURL)),
		transport: http.DefaultTransport,
		metrics:   newMetrics(),
		recent:    newRecentErrors(8),
	}
	srv := httptest.NewServer(http.HandlerFunc(h.ServeHTTP))
	t.Cleanup(srv.Close)

	for _, path := range []string{"/admin/accounts/ag-leak/verify", "/admin/accounts/verify"} {
		t.Run(path, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodPost, srv.URL+path, nil)
			req.Header.Set("X-Admin-Token", "admin-token-test")
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			body, _ := io.ReadAll(resp.Body)
			if resp.StatusCode != http.StatusOK {
				t.Fatalf("%s: status %d, body %s", path, resp.StatusCode, body)
			}
			if !strings.Contains(string(body), "needs_verification") {
				t.Fatalf("%s: probe did not classify the forbidden fixture, body %s", path, body)
			}
			for _, marker := range []string{accessMarker, refreshMarker} {
				if strings.Contains(string(body), marker) {
					t.Fatalf("%s leaked upstream credential marker %q", path, marker)
				}
			}
		})
	}
}
