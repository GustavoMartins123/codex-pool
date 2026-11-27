package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"codex-pool-proxy/internal/accountstate"
	"codex-pool-proxy/internal/routing"
)

func placeholderTestHandler(t *testing.T) *proxyHandler {
	t.Helper()
	base, _ := url.Parse("https://upstream.example")
	h := &proxyHandler{
		cfg:      &config{poolDir: t.TempDir()},
		registry: NewProviderRegistry(NewCodexProvider(base, base, base), NewClaudeProvider(base), nil, NewAntigravityProvider(base, base), NewKimiProvider(base)),
	}
	attachContributionFixture(t, h, "operator")
	h.passportConfig = passportConfig{jwtSecret: strings.Repeat("placeholder-test-secret-", 3)}
	return h
}

func writeTestAccountFile(t *testing.T, h *proxyHandler, rel string, doc map[string]any) string {
	t.Helper()
	path := filepath.Join(h.cfg.poolDir, rel)
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(doc)
	if err != nil {
		t.Fatal(err)
	}
	if err := h.cfg.vault.writeAccountFile(path, data); err != nil {
		t.Fatal(err)
	}
	return path
}

func testAccountByID(t *testing.T, h *proxyHandler, id string) *Account {
	t.Helper()
	for _, acc := range h.pool.allAccounts() {
		if acc.ID == id {
			return acc
		}
	}
	t.Fatalf("account %q is not loaded", id)
	return nil
}

func TestLoadPoolBuildsPlaceholderAccounts(t *testing.T) {
	h := placeholderTestHandler(t)
	writeTestAccountFile(t, h, "codex/codex_waiting.json", map[string]any{"placeholder": true, "email": " One@Example.com ", "plan_type": "pro", "added_at": "2026-11-01T00:00:00Z"})
	writeTestAccountFile(t, h, "antigravity/antigravity-waiting.json", map[string]any{"placeholder": true, "auth_error": "custom reason"})
	writeTestAccountFile(t, h, "claude/claude_waiting.json", map[string]any{"placeholder": true})
	writeTestAccountFile(t, h, "codex/codex_ready.json", map[string]any{"placeholder": true, "tokens": map[string]any{"access_token": jwtWithExp(time.Now().Add(time.Hour).Unix()), "refresh_token": "refresh-ready"}})
	writeTestAccountFile(t, h, "codex/codex_unmarked.json", map[string]any{"email": "nobody@example.com"})

	if err := h.reloadAccounts(); err != nil {
		t.Fatal(err)
	}
	if got := len(h.pool.allAccounts()); got != 4 {
		t.Fatalf("loaded %d accounts, want 4", got)
	}

	waiting := testAccountByID(t, h, "codex_waiting")
	if !isPlaceholderAccount(waiting) {
		t.Fatal("codex_waiting is not a placeholder")
	}
	if waiting.Type != AccountTypeCodex || !waiting.NeedsLogin || waiting.AuthError != placeholderAccountAuthError {
		t.Fatalf("waiting account state: type=%s needs_login=%v auth_error=%q", waiting.Type, waiting.NeedsLogin, waiting.AuthError)
	}
	if waiting.AccessToken != "" || waiting.RefreshToken != "" || waiting.IDToken != "" {
		t.Fatal("a placeholder carries a credential")
	}
	if waiting.Email != "One@Example.com" || waiting.PlanType != "pro" {
		t.Fatalf("placeholder identity: email=%q plan=%q", waiting.Email, waiting.PlanType)
	}
	if want := time.Date(2026, 11, 1, 0, 0, 0, 0, time.UTC); !waiting.AddedAt.Equal(want) {
		t.Fatalf("added_at=%s want %s", waiting.AddedAt, want)
	}
	if waiting.File != filepath.Join(h.cfg.poolDir, "codex", "codex_waiting.json") {
		t.Fatalf("placeholder file=%s", waiting.File)
	}

	antigravity := testAccountByID(t, h, "antigravity-waiting")
	if antigravity.Type != AccountTypeAntigravity || !isPlaceholderAccount(antigravity) || antigravity.AuthError != "custom reason" {
		t.Fatalf("antigravity placeholder: type=%s placeholder=%v auth_error=%q", antigravity.Type, isPlaceholderAccount(antigravity), antigravity.AuthError)
	}
	if claude := testAccountByID(t, h, "claude_waiting"); claude.Type != AccountTypeClaude || !isPlaceholderAccount(claude) {
		t.Fatalf("claude placeholder: type=%s placeholder=%v", claude.Type, isPlaceholderAccount(claude))
	}

	// The marker stops mattering as soon as the file holds a credential.
	ready := testAccountByID(t, h, "codex_ready")
	if isPlaceholderAccount(ready) || ready.NeedsLogin || ready.RefreshToken != "refresh-ready" || ready.AccessToken == "" {
		t.Fatalf("an account with credentials was loaded as a placeholder: placeholder=%v needs_login=%v", isPlaceholderAccount(ready), ready.NeedsLogin)
	}
	for _, acc := range h.pool.allAccounts() {
		if acc.ID == "codex_unmarked" {
			t.Fatal("a file without credentials and without the marker was loaded")
		}
	}
}

func TestPlaceholderAccountStaysOutOfRouting(t *testing.T) {
	pool := newPoolState(nil, false)
	waiting := &Account{Type: AccountTypeCodex, ID: "codex_waiting", NeedsLogin: true, placeholder: true}
	rejected := &Account{Type: AccountTypeCodex, ID: "codex_rejected", AccessToken: "token", NeedsLogin: true}
	pool.replace([]*Account{waiting, rejected})

	if got := pool.selectRoute(routing.Request{Mode: routing.MatchAccountID, AccountID: waiting.ID, Provider: string(AccountTypeCodex)}).Account; got != nil {
		t.Fatalf("the placeholder was selected by id: %s", got.ID)
	}
	// A rejected refresh keeps serving while its access token lives; a
	// placeholder has none and is never chosen.
	for range 50 {
		if got := pool.selectRoute(routing.Request{Provider: string(AccountTypeCodex)}).Account; got != rejected {
			t.Fatalf("eligible routing chose %v", got)
		}
	}

	now := time.Now()
	waiting.mu.Lock()
	waitingState := accountStateSnapshotLocked(nil, waiting, now)
	waitingHealth := accountHealthBlockedLocked(waiting)
	waiting.mu.Unlock()
	rejected.mu.Lock()
	rejectedState := accountStateSnapshotLocked(nil, rejected, now)
	rejectedHealth := accountHealthBlockedLocked(rejected)
	rejected.mu.Unlock()

	if waitingState.State != accountstate.StateNeedsLogin || waitingState.Routable || !waitingHealth {
		t.Fatalf("placeholder state=%s routable=%v health_blocked=%v", waitingState.State, waitingState.Routable, waitingHealth)
	}
	if rejectedState.State != accountstate.StateNeedsLogin || !rejectedState.Routable || rejectedHealth {
		t.Fatalf("rejected refresh state=%s routable=%v health_blocked=%v", rejectedState.State, rejectedState.Routable, rejectedHealth)
	}
}

func TestCodexReloginClaimsPlaceholderAccount(t *testing.T) {
	h := placeholderTestHandler(t)
	path := writeTestAccountFile(t, h, "codex/codex_waiting.json", map[string]any{"placeholder": true, "email": "a@example.com", "plan_type": "pro"})
	if err := h.reloadAccounts(); err != nil {
		t.Fatal(err)
	}

	tokens := codexTestTokens(t, testCodexWorkspace, "user-a", "a@example.com")
	if err := h.replaceCodexAccountCredentials("codex_waiting", tokens); err != nil {
		t.Fatalf("sign-in into the placeholder: %v", err)
	}

	raw, err := h.cfg.vault.readAccountFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	for _, key := range []string{placeholderAccountMarker, "needs_login", "auth_error", "dead"} {
		if _, ok := root[key]; ok {
			t.Fatalf("%q survived the sign-in", key)
		}
	}
	stored, _ := root["tokens"].(map[string]any)
	if stored["refresh_token"] != tokens.RefreshToken || stored["id_token"] != tokens.IDToken {
		t.Fatalf("tokens were not stored: %v", stored)
	}
	if root["plan_type"] != "pro" || root["email"] != "a@example.com" {
		t.Fatalf("imported account settings were dropped: %v", root)
	}

	if err := h.reloadAccounts(); err != nil {
		t.Fatal(err)
	}
	acc := testAccountByID(t, h, "codex_waiting")
	if isPlaceholderAccount(acc) || acc.NeedsLogin || acc.AccessToken != tokens.AccessToken || acc.Email != "a@example.com" {
		t.Fatalf("account after sign-in: placeholder=%v needs_login=%v", isPlaceholderAccount(acc), acc.NeedsLogin)
	}
	// From now on the account has an identity, and a teammate cannot take it.
	if err := h.replaceCodexAccountCredentials("codex_waiting", codexTestTokens(t, testCodexWorkspace, "user-b", "b@example.com")); err == nil {
		t.Fatal("another workspace member replaced a claimed account")
	}
}

func TestClaudeReloginClaimsPlaceholderAccount(t *testing.T) {
	h := placeholderTestHandler(t)
	path := writeTestAccountFile(t, h, "claude/claude_waiting.json", map[string]any{"placeholder": true, "email": "a@example.com"})
	if err := h.reloadAccounts(); err != nil {
		t.Fatal(err)
	}
	target := testAccountByID(t, h, "claude_waiting")

	authority := h.passport
	h.passport = nil
	request := contributionRequestActor(httptest.NewRequest(http.MethodPost, "/admin/claude/relogin", strings.NewReader(`{"account_id":"claude_waiting"}`)), "operator")
	response := httptest.NewRecorder()
	if got := h.claudeReloginTarget(response, request, "claude_waiting"); got == nil {
		h.passport = authority
		t.Fatalf("relogin refused the placeholder: %d %s", response.Code, response.Body.String())
	}
	h.passport = authority

	fresh := &ClaudeTokenResponse{AccessToken: "sk-ant-oat-fresh", RefreshToken: "refresh-fresh", ExpiresIn: 3600}
	if err := replaceClaudeAccountCredentials(target, fresh, ""); err == nil {
		t.Fatal("a sign-in without an account UUID claimed the placeholder")
	}
	if err := replaceClaudeAccountCredentials(target, fresh, "acct-1"); err != nil {
		t.Fatalf("sign-in into the placeholder: %v", err)
	}

	raw, err := h.cfg.vault.readAccountFile(path)
	if err != nil {
		t.Fatal(err)
	}
	var root map[string]any
	if err := json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	if root["account_uuid"] != "acct-1" {
		t.Fatalf("account_uuid=%v", root["account_uuid"])
	}
	oauth, _ := root["claudeAiOauth"].(map[string]any)
	if oauth["accessToken"] != fresh.AccessToken || oauth["refreshToken"] != fresh.RefreshToken {
		t.Fatalf("credentials were not stored: %v", oauth)
	}
	for _, key := range []string{placeholderAccountMarker, "needs_login", "auth_error"} {
		if _, ok := root[key]; ok {
			t.Fatalf("%q survived the sign-in", key)
		}
	}

	if err := h.reloadAccounts(); err != nil {
		t.Fatal(err)
	}
	acc := testAccountByID(t, h, "claude_waiting")
	if isPlaceholderAccount(acc) || acc.NeedsLogin || acc.AccessToken != fresh.AccessToken || acc.claude.UUID != "acct-1" {
		t.Fatalf("account after sign-in: placeholder=%v needs_login=%v uuid=%s", isPlaceholderAccount(acc), acc.NeedsLogin, acc.claude.UUID)
	}
	other := &ClaudeTokenResponse{AccessToken: "sk-ant-oat-other", RefreshToken: "refresh-other", ExpiresIn: 3600}
	if err := replaceClaudeAccountCredentials(acc, other, "acct-2"); err == nil {
		t.Fatal("a different Claude account replaced a claimed account")
	}
}

func TestAntigravityReloginMatchesTarget(t *testing.T) {
	target := &Account{Type: AccountTypeAntigravity, ID: "antigravity_16993c93e28fb292702f5181", Email: " Person@Example.com "}
	for _, tc := range []struct {
		name, signedInID, email string
		want                    bool
	}{
		{"same email, Passport-named account", "antigravity-person-example.com", "person@example.com", true},
		{"same id", target.ID, "", true},
		{"different email", "antigravity-other-example.com", "other@example.com", false},
		{"no recorded email", "antigravity-person-example.com", "person@example.com", false},
		{"no target", "antigravity-person-example.com", "person@example.com", false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			candidate := target
			switch {
			case tc.name == "no recorded email":
				candidate = &Account{Type: AccountTypeAntigravity, ID: target.ID}
			case tc.name == "no target":
				candidate = nil
			}
			if got := antigravityReloginMatchesTarget(candidate, tc.signedInID, tc.email); got != tc.want {
				t.Fatalf("match=%v want %v", got, tc.want)
			}
		})
	}
}

func TestAccountVerifyRefusesPlaceholder(t *testing.T) {
	h := placeholderTestHandler(t)
	writeTestAccountFile(t, h, "codex/codex_waiting.json", map[string]any{"placeholder": true})
	if err := h.reloadAccounts(); err != nil {
		t.Fatal(err)
	}

	single := httptest.NewRecorder()
	h.handleAccountVerify(single, contributionRequestActor(httptest.NewRequest(http.MethodPost, "/admin/accounts/codex_waiting/verify", nil), "operator"), "codex_waiting")
	if single.Code != http.StatusBadRequest || !strings.Contains(single.Body.String(), "sign in") {
		t.Fatalf("verify status=%d body=%s", single.Code, single.Body.String())
	}

	batch := httptest.NewRecorder()
	h.handleAccountVerifyBatch(batch, contributionRequestActor(httptest.NewRequest(http.MethodPost, "/admin/accounts/verify", nil), "operator"))
	if batch.Code != http.StatusOK || strings.Contains(batch.Body.String(), "codex_waiting") {
		t.Fatalf("batch verify status=%d body=%s", batch.Code, batch.Body.String())
	}
}
