package main

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

func TestNewAntigravityOAuthSessionUsesPKCESafeValues(t *testing.T) {
	session, err := newAntigravityOAuthSession()
	if err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]string{"id": session.ID, "state": session.State, "verifier": session.Verifier} {
		if value == "" {
			t.Fatalf("%s was empty", name)
		}
		if _, err := base64.RawURLEncoding.DecodeString(value); err != nil {
			t.Fatalf("%s is not base64url: %v", name, err)
		}
	}
}

func TestAntigravityAddUsesConfiguredOAuthIdentity(t *testing.T) {
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_ID", "test-client-id")
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_SECRET", "test-client-secret")
	old := os.Getenv("ANTIGRAVITY_OAUTH_REDIRECT_URI")
	t.Cleanup(func() { _ = os.Setenv("ANTIGRAVITY_OAUTH_REDIRECT_URI", old) })
	_ = os.Unsetenv("ANTIGRAVITY_OAUTH_REDIRECT_URI")
	if got := antigravityOAuthRedirectURI(); got != antigravityOAuthCallbackURL {
		t.Fatalf("redirect URI = %q", got)
	}
	if got := antigravityOAuthClientID(); got != "test-client-id" {
		t.Fatalf("client ID = %q", got)
	}
	if got := antigravityOAuthClientSecret(); got != "test-client-secret" {
		t.Fatalf("client secret = %q", got)
	}
	if got := antigravityUserAgent(); got != "antigravity/hub/2.2.1 darwin/arm64" {
		t.Fatalf("user agent = %q", got)
	}
}

func TestAntigravityAddBuildsRealGoogleAuthorizationURL(t *testing.T) {
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_ID", "test-client-id")
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_SECRET", "")
	t.Setenv("ANTIGRAVITY_OAUTH_REDIRECT_URI", "https://pool.example.test/admin/antigravity/callback")
	request := httptest.NewRequest(http.MethodPost, "/api/pool/accounts/antigravity/add", strings.NewReader("{}"))
	request.Header.Set("Origin", "https://pool.example.test")
	recorder := httptest.NewRecorder()
	h := &proxyHandler{}
	attachContributionFixture(t,h,"alice")
	h.handleAntigravityAdd(recorder, contributionRequestActor(request,"alice"))
	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected response %d %s", recorder.Code, recorder.Body.String())
	}
	var result struct {
		OAuthURL  string `json:"oauth_url"`
		SessionID string `json:"session_id"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	authorize, err := url.Parse(result.OAuthURL)
	if err != nil {
		t.Fatal(err)
	}
	query := authorize.Query()
	if authorize.Scheme+"://"+authorize.Host+authorize.Path != antigravityOAuthAuthorizeURL || query.Get("client_id") != "test-client-id" || query.Get("redirect_uri") != "https://pool.example.test/admin/antigravity/callback" {
		t.Fatalf("unexpected authorization URL %s", result.OAuthURL)
	}
	if query.Get("access_type") != "offline" || query.Get("prompt") != "consent" || query.Get("code_challenge_method") != "S256" {
		t.Fatalf("missing OAuth parameters: %v", query)
	}
	antigravityOAuthSessions.Lock()
	if session := antigravityOAuthSessions.byID[result.SessionID]; session != nil {
		delete(antigravityOAuthSessions.byState, session.State)
	}
	delete(antigravityOAuthSessions.byID, result.SessionID)
	antigravityOAuthSessions.Unlock()
}

func TestAntigravityReloginBindsOAuthSessionToAccount(t *testing.T) {
	t.Setenv("ANTIGRAVITY_OAUTH_CLIENT_ID", "test-client-id")
	t.Setenv("ANTIGRAVITY_OAUTH_REDIRECT_URI", "https://pool.example.test/admin/antigravity/callback")
	account := &Account{Type: AccountTypeAntigravity, ID: "relogin-account", Email: "target@example.com", File: filepath.Join(t.TempDir(), "account.json")}
	handler := &proxyHandler{pool: newPoolState([]*Account{account}, false), cfg: &config{}}
	request := httptest.NewRequest(http.MethodPost, "/admin/antigravity/relogin", strings.NewReader(`{"account_id":"relogin-account"}`))
	request.Header.Set("Origin", "https://pool.example.test")
	recorder := httptest.NewRecorder()
	handler.handleAntigravityRelogin(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("unexpected response %d %s", recorder.Code, recorder.Body.String())
	}
	var result struct {
		SessionID string `json:"session_id"`
		OAuthURL  string `json:"oauth_url"`
		LoginHint bool   `json:"login_hint"`
	}
	if err := json.Unmarshal(recorder.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	parsed, err := url.Parse(result.OAuthURL)
	if err != nil {
		t.Fatal(err)
	}
	if hint := parsed.Query().Get("login_hint"); hint != "target@example.com" {
		t.Fatalf("oauth url did not preselect the account email: %q", hint)
	}
	if !result.LoginHint {
		t.Fatal("expected login_hint to be reported")
	}
	antigravityOAuthSessions.Lock()
	session := antigravityOAuthSessions.byID[result.SessionID]
	antigravityOAuthSessions.Unlock()
	if session == nil || session.ReloginAccountID != account.ID || session.ActorID != "" {
		t.Fatalf("OAuth session was not bound to account: %+v", session)
	}
	antigravityOAuthSessions.Lock()
	delete(antigravityOAuthSessions.byID, session.ID)
	delete(antigravityOAuthSessions.byState, session.State)
	antigravityOAuthSessions.Unlock()
}

func TestReplaceAntigravityAccountCredentialsClearsVerification(t *testing.T) {
	file := filepath.Join(t.TempDir(), "account.json")
	account := &Account{
		Type: AccountTypeAntigravity, ID: "replace-account", File: file,
		AccessToken: "old-access", RefreshToken: "old-refresh", ProjectID: "old-project",
		PlanType: "pro", Disabled: true, Dead: true, NeedsVerification: true,
		VerificationURL: "https://verify.example", HealthError: "403", ModelRateLimits: make(map[string]time.Time),
	}
	snapshot := AntigravityAccountSnapshot{FetchedAt: time.Now(), Models: map[string]AntigravityModelInfo{"gemini-test": {ID: "gemini-test"}}}
	antigravityModels.ReplaceAccount(account.ID, snapshot)
	if err := (&proxyHandler{}).replaceAntigravityAccountCredentials(account, antigravityTokenResponse{AccessToken: "new-access", RefreshToken: "new-refresh", ExpiresIn: 3600}, "user@example.com", "new-project", "ultra", snapshot); err != nil {
		t.Fatal(err)
	}
	if account.AccessToken != "new-access" || account.RefreshToken != "new-refresh" || account.ProjectID != "new-project" || account.PlanType != "ultra" {
		t.Fatalf("credentials were not replaced: %+v", account)
	}
	if account.NeedsVerification || account.VerificationURL != "" || account.HealthError != "" || account.Dead || !account.Disabled {
		t.Fatalf("health state was not cleared safely: %+v", account)
	}
	var saved AntigravityAuthJSON
	raw, err := os.ReadFile(file)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(raw, &saved); err != nil {
		t.Fatal(err)
	}
	if saved.NeedsVerification || saved.VerificationURL != "" || saved.HealthError != "" || saved.AccessToken != "new-access" {
		t.Fatalf("persisted health state was not cleared: %+v", saved)
	}
}

func TestAntigravityReloginValidationRejectsVerificationAndAcceptsQuota(t *testing.T) {
	daily, _ := url.Parse("https://daily.example")
	production, _ := url.Parse("https://prod.example")
	provider := NewAntigravityProvider(daily, production)
	account := &Account{ProjectID: "project", File: filepath.Join(t.TempDir(), "account.json")}
	snapshot := AntigravityAccountSnapshot{Models: map[string]AntigravityModelInfo{"gemini-test": {ID: "gemini-test"}}}

	verificationHandler := &proxyHandler{transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusForbidden, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"code":403,"message":"Verify your account to continue.","status":"PERMISSION_DENIED"}}`)), Request: req}, nil
	})}
	if _, err := verificationHandler.validateAntigravityRelogin(context.Background(), account, account, provider, snapshot); err == nil {
		t.Fatal("verification-required response was accepted")
	}
	if !account.NeedsVerification || account.HealthError == "" {
		t.Fatal("verification failure was not persisted on the target account")
	}

	quotaHandler := &proxyHandler{transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return &http.Response{StatusCode: http.StatusTooManyRequests, Header: make(http.Header), Body: io.NopCloser(strings.NewReader(`{"error":{"code":429,"message":"Resource has been exhausted","status":"RESOURCE_EXHAUSTED","details":[{"reason":"QUOTA_EXCEEDED"}]}}`)), Request: req}, nil
	})}
	quotaLimited, err := quotaHandler.validateAntigravityRelogin(context.Background(), account, account, provider, snapshot)
	if err != nil || !quotaLimited {
		t.Fatalf("quota response was not classified as quota: quota=%t err=%v", quotaLimited, err)
	}
}

func TestSafeAntigravityAccountID(t *testing.T) {
	if got := safeAntigravityAccountID("Person+AI@Example.COM"); got != "antigravity-person-ai-example.com" {
		t.Fatalf("got %q", got)
	}
}

func TestAntigravityManualExchangeRequiresMatchingState(t *testing.T) {
	session, err := newAntigravityOAuthSession()
	if err != nil {
		t.Fatal(err)
	}
	antigravityOAuthSessions.Lock()
	antigravityOAuthSessions.byID[session.ID] = session
	antigravityOAuthSessions.byState[session.State] = session
	antigravityOAuthSessions.Unlock()
	t.Cleanup(func() {
		antigravityOAuthSessions.Lock()
		delete(antigravityOAuthSessions.byID, session.ID)
		delete(antigravityOAuthSessions.byState, session.State)
		antigravityOAuthSessions.Unlock()
	})
	body, _ := json.Marshal(map[string]string{"session_id": session.ID, "code": "code", "state": "wrong"})
	recorder := httptest.NewRecorder()
	(&proxyHandler{}).handleAntigravityExchange(recorder, httptest.NewRequest(http.MethodPost, "/api/pool/accounts/antigravity/exchange", strings.NewReader(string(body))))
	if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), "state") {
		t.Fatalf("unexpected response %d %s", recorder.Code, recorder.Body.String())
	}
}

func TestAntigravityProjectIDUsesKnownShapes(t *testing.T) {
	root := map[string]any{"unrelated": map[string]any{"project": "wrong"}, "cloudaicompanionProject": map[string]any{"id": "right"}}
	if got := antigravityLoadProjectID(root); got != "right" {
		t.Fatalf("got %q", got)
	}
}

func TestAntigravityOnboardProjectRequiresCompletion(t *testing.T) {
	response := map[string]any{"cloudaicompanionProject": map[string]any{"id": "project"}}
	if got := antigravityOnboardProjectID(map[string]any{"done": false, "response": response}); got != "" {
		t.Fatalf("incomplete onboarding returned %q", got)
	}
	if got := antigravityOnboardProjectID(map[string]any{"done": true, "response": response}); got != "project" {
		t.Fatalf("completed onboarding returned %q", got)
	}
}

func TestSaveAntigravityAccountIsOwnerOnlyAndDurable(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "account.json")
	account := &Account{Type: AccountTypeAntigravity, ID: "account", File: file, AccessToken: "access", RefreshToken: "refresh", Email: "a@example.com", ProjectID: "project", PlanType: "pro", ExpiresAt: time.Now().Add(time.Hour), ModelRateLimits: make(map[string]time.Time)}
	antigravityModels.ReplaceAccount(account.ID, AntigravityAccountSnapshot{FetchedAt: time.Now(), Models: map[string]AntigravityModelInfo{"gemini-test": {ID: "gemini-test"}}})
	if err := saveAntigravityAccount(account); err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(file)
	if err != nil {
		t.Fatal(err)
	}
	if runtime.GOOS != "windows" && info.Mode().Perm() != 0o600 {
		t.Fatalf("mode is %o", info.Mode().Perm())
	}
	var saved AntigravityAuthJSON
	raw, _ := os.ReadFile(file)
	if err := json.Unmarshal(raw, &saved); err != nil || saved.ProjectID != "project" || saved.ModelSnapshot == nil {
		t.Fatalf("bad saved credential: %v %#v", err, saved)
	}
}

func TestAntigravityAccountBoundVerificationURL(t *testing.T) {
	raw := "https://accounts.google.com/signin/continue?continue=https%3A%2F%2Fdevelopers.google.com%2Fgemini-code-assist%2Fauth%2Fauth_success_gemini&flowName=GlifWebSignIn&scc=1"
	bound := antigravityAccountBoundVerificationURL(raw, "target@example.com")
	parsed, err := url.Parse(bound)
	if err != nil {
		t.Fatal(err)
	}
	if parsed.Host != "accounts.google.com" || parsed.Path != "/AccountChooser" {
		t.Fatalf("verification url was not routed through the account chooser: %s", bound)
	}
	query := parsed.Query()
	if query.Get("Email") != "target@example.com" {
		t.Fatalf("chooser is not bound to the account e-mail: %q", query.Get("Email"))
	}
	if query.Get("continue") != raw {
		t.Fatalf("upstream verification flow was not preserved: %q", query.Get("continue"))
	}
	if got := antigravityAccountBoundVerificationURL(raw, "  "); got != raw {
		t.Fatalf("missing e-mail must leave the url untouched: %q", got)
	}
	foreign := "https://support.example.test/appeal/abc"
	if got := antigravityAccountBoundVerificationURL(foreign, "target@example.com"); got != foreign {
		t.Fatalf("non-Google url must be preserved: %q", got)
	}
}
