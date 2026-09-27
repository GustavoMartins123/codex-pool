package main

import (
	"io"
	"net/http"
	"net/url"
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
