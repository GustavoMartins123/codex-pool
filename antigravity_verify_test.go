package main

import (
	"context"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codex-pool-proxy/internal/logredact"
)

const antigravityTOSBanFixture = `{"error":{"code":403,"status":"PERMISSION_DENIED","message":"This service has been disabled in this account for violation of Terms of Service.","details":[{"@type":"type.googleapis.com/google.rpc.ErrorInfo","reason":"TOS_VIOLATION","metadata":{"appeal_url":"https://forms.gle/appeal?continue=AbCdEf0123456789_-x"}}]}}`

const antigravityExhaustedQuotaFixture = `{"groups":[{"displayName":"Gemini Models","buckets":[{"bucketId":"gemini-5h","window":"5h","resetTime":"2099-01-01T00:00:00Z"},{"bucketId":"gemini-weekly","window":"weekly","remainingFraction":0,"remainingAmount":0,"resetTime":"2099-01-01T00:00:00Z"}]}]}`

func antigravityVerifyTestAccount(t *testing.T, id string) *Account {
	t.Helper()
	return &Account{
		Type: AccountTypeAntigravity, ID: id,
		vault:        plainTestVault(),
		File:         filepath.Join(t.TempDir(), id+".json"),
		AccessToken:  "token",
		RefreshToken: "refresh",
		google:       googleAccountState{ProjectID: "project", ModelRateLimits: map[string]time.Time{}},
		ExpiresAt:    time.Now().Add(time.Hour),
	}
}

func antigravityVerifyFixtureHandler(t *testing.T, accounts []*Account, transport http.RoundTripper) (*proxyHandler, *url.URL, *url.URL) {
	t.Helper()
	daily, _ := url.Parse("https://daily.example.test")
	production, _ := url.Parse("https://prod.example.test")
	base, _ := url.Parse("https://unused.example.test")
	h := &proxyHandler{
		cfg:              &config{},
		pool:             newPoolState(accounts, false),
		registry:         NewProviderRegistry(NewCodexProvider(base, base, base), NewClaudeProvider(base), NewGeminiProvider(base, base), NewAntigravityProvider(daily, production)),
		transport:        transport,
		refreshTransport: transport,
	}
	return h, daily, production
}

func TestAntigravityVerifyDetectsTOSBan(t *testing.T) {
	account := antigravityVerifyTestAccount(t, "ag-banned")
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "prod.example.test" {
			return antigravityTestResponse(http.StatusOK, antigravityHealthyQuotaFixture), nil
		}
		return antigravityTestResponse(http.StatusForbidden, antigravityTOSBanFixture), nil
	})
	h, daily, production := antigravityVerifyFixtureHandler(t, []*Account{account}, transport)

	result := h.verifyAntigravityAccount(context.Background(), account, daily, production)

	if result.Status != "banned" {
		t.Fatalf("status = %q, want banned (detail %q)", result.Status, result.Detail)
	}
	if !account.Dead {
		t.Fatal("a TOS ban must mark the account dead so routing stops")
	}
	if result.AppealURL != "https://forms.gle/appeal?continue=AbCdEf0123456789_-x" {
		t.Fatalf("appeal URL = %q", result.AppealURL)
	}
	if !strings.Contains(result.Detail, "PERMISSION_DENIED") {
		t.Fatalf("detail lost the upstream error summary: %q", result.Detail)
	}
	if account.HealthError == "" {
		t.Fatal("ban body must be persisted as the health error")
	}
	if _, err := os.Stat(account.File); err != nil {
		t.Fatalf("account was not persisted: %v", err)
	}
}

func TestAntigravityVerifyDetectsValidationRequired(t *testing.T) {
	account := antigravityVerifyTestAccount(t, "ag-verify")
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return antigravityTestResponse(http.StatusForbidden, antigravityVerifyFixture), nil
	})
	h, daily, production := antigravityVerifyFixtureHandler(t, []*Account{account}, transport)

	result := h.verifyAntigravityAccount(context.Background(), account, daily, production)

	if result.Status != "needs_verification" {
		t.Fatalf("status = %q, want needs_verification", result.Status)
	}
	if !account.NeedsVerification || account.VerificationURL != "https://accounts.google.com/verify" {
		t.Fatalf("verification state = %v %q", account.NeedsVerification, account.VerificationURL)
	}
	if result.VerificationURL != "https://accounts.google.com/verify" {
		t.Fatalf("verification URL = %q", result.VerificationURL)
	}
	if account.Dead {
		t.Fatal("a verification demand must not kill the account")
	}
}

func TestAntigravityVerifyReportsExhaustedQuota(t *testing.T) {
	account := antigravityVerifyTestAccount(t, "ag-empty")
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return antigravityTestResponse(http.StatusOK, antigravityExhaustedQuotaFixture), nil
	})
	h, daily, production := antigravityVerifyFixtureHandler(t, []*Account{account}, transport)

	result := h.verifyAntigravityAccount(context.Background(), account, daily, production)

	if result.Status != "quota_exhausted" {
		t.Fatalf("status = %q, want quota_exhausted", result.Status)
	}
	if account.Dead || account.NeedsVerification {
		t.Fatal("exhausted quota is not a health failure")
	}
}

func TestAntigravityVerifyHealthyClearsMarkers(t *testing.T) {
	account := antigravityVerifyTestAccount(t, "ag-ok")
	account.NeedsVerification = true
	account.VerificationURL = "https://accounts.google.com/verify"
	account.HealthError = "stale error"
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return antigravityTestResponse(http.StatusOK, antigravityHealthyQuotaFixture), nil
	})
	h, daily, production := antigravityVerifyFixtureHandler(t, []*Account{account}, transport)

	result := h.verifyAntigravityAccount(context.Background(), account, daily, production)

	if result.Status != "healthy" {
		t.Fatalf("status = %q, want healthy", result.Status)
	}
	if account.NeedsVerification || account.VerificationURL != "" || account.HealthError != "" {
		t.Fatalf("healthy answer must clear stale markers: %v %q %q", account.NeedsVerification, account.VerificationURL, account.HealthError)
	}
	if _, err := os.Stat(account.File); err != nil {
		t.Fatalf("cleared state was not persisted: %v", err)
	}
}

func TestAntigravityVerifyRevokedRefreshToken(t *testing.T) {
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_ID", "test-client-id")
	account := antigravityVerifyTestAccount(t, "ag-revoked")
	account.ExpiresAt = time.Now().Add(-time.Minute)
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.Host == "oauth2.googleapis.com" {
			return antigravityTestResponse(http.StatusBadRequest, `{"error":"invalid_grant","error_description":"Token has been expired or revoked."}`), nil
		}
		return antigravityTestResponse(http.StatusOK, antigravityHealthyQuotaFixture), nil
	})
	h, daily, production := antigravityVerifyFixtureHandler(t, []*Account{account}, transport)

	result := h.verifyAntigravityAccount(context.Background(), account, daily, production)

	if result.Status != "revoked" {
		t.Fatalf("status = %q, want revoked (detail %q)", result.Status, result.Detail)
	}
	if !account.Dead {
		t.Fatal("a revoked refresh token must mark the account dead")
	}
}

func TestAntigravityVerifyUnclassifiedForbiddenIsError(t *testing.T) {
	account := antigravityVerifyTestAccount(t, "ag-unknown")
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return antigravityTestResponse(http.StatusForbidden, `{"error":{"code":403,"status":"PERMISSION_DENIED","message":"Forbidden."}}`), nil
	})
	h, daily, production := antigravityVerifyFixtureHandler(t, []*Account{account}, transport)

	result := h.verifyAntigravityAccount(context.Background(), account, daily, production)

	if result.Status != "error" {
		t.Fatalf("status = %q, want error for an unclassified 403", result.Status)
	}
	if account.Dead || account.NeedsVerification {
		t.Fatal("an unclassified 403 must not assert a ban or verification demand")
	}
	if !account.RateLimitUntil.After(time.Now()) {
		t.Fatal("an unclassified 403 must back the account off temporarily")
	}
}

func TestClassifyAntigravityForbiddenTOSReason(t *testing.T) {
	if _, banned, _ := classifyAntigravityForbidden([]byte(`{"error":{"details":[{"reason":"TOS_VIOLATION"}]}}`)); !banned {
		t.Fatal("TOS_VIOLATION reason must classify as a ban without message text")
	}
	if _, banned, _ := classifyAntigravityForbidden([]byte(`{"error":{"message":"This service has been disabled in this account for violation of the acceptable use policy."}}`)); !banned {
		t.Fatal("disabled-for-violation message must classify as a ban")
	}
	if needsVerification, _, _ := classifyAntigravityForbidden([]byte(antigravityVerifyFixture)); !needsVerification {
		t.Fatal("validation demand classification regressed")
	}
}

func TestSafeAntigravityAppealURLAllowlist(t *testing.T) {
	allowed := []string{
		"https://forms.gle/appeal",
		"https://accounts.google.com/verify",
		"https://docs.google.com/forms/d/e/abc/viewform",
		"https://forms.gle:443/appeal",
	}
	for _, raw := range allowed {
		if got := safeAntigravityAppealURL(raw); got == "" {
			t.Fatalf("allowlisted URL was dropped: %q", raw)
		}
	}
	rejected := []string{
		"",
		"not a url",
		"http://forms.gle/appeal",
		"https://evil.example/forms.gle",
		"https://forms.gle.evil.example/appeal",
		"https://user:pass@forms.gle/appeal",
		"https://forms.gle:8443/appeal",
	}
	for _, raw := range rejected {
		if got := safeAntigravityAppealURL(raw); got != "" {
			t.Fatalf("URL %q must be dropped, got %q", raw, got)
		}
	}
}

func TestAntigravityAppealContinuationTokenRedacted(t *testing.T) {
	logged := logredact.Redact("appeal link: https://forms.gle/appeal?continue=AbCdEf0123456789_-x")
	if strings.Contains(logged, "AbCdEf0123456789_-x") {
		t.Fatalf("continuation token reached the log: %s", logged)
	}
	if !strings.Contains(logged, "continue=<redacted>") {
		t.Fatalf("expected redacted continuation parameter: %s", logged)
	}
}
