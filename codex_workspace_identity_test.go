package main

import (
	"encoding/base64"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

const testCodexWorkspace = "11111111-2222-3333-4444-555555555555"

func codexTestIDToken(t *testing.T, workspace, userID, email string) string {
	t.Helper()
	auth := map[string]any{"chatgpt_account_id": workspace, "chatgpt_plan_type": "team"}
	if userID != "" {
		auth["chatgpt_user_id"] = userID
		auth["user_id"] = userID
	}
	claims := map[string]any{"exp": time.Now().Add(time.Hour).Unix(), "https://api.openai.com/auth": auth}
	if email != "" {
		claims["email"] = email
	}
	payload, err := json.Marshal(claims)
	if err != nil {
		t.Fatal(err)
	}
	return "h." + base64.RawURLEncoding.EncodeToString(payload) + ".s"
}

func codexTestTokens(t *testing.T, workspace, userID, email string) *CodexTokenResponse {
	t.Helper()
	return &CodexTokenResponse{IDToken: codexTestIDToken(t, workspace, userID, email), AccessToken: jwtWithExp(time.Now().Add(time.Hour).Unix()), RefreshToken: "refresh-" + userID + email}
}

func codexContributionHandler(t *testing.T) *proxyHandler {
	t.Helper()
	base, _ := url.Parse("https://chatgpt.example")
	h := &proxyHandler{cfg: &config{poolDir: t.TempDir()}, registry: NewProviderRegistry(NewCodexProvider(base, base, base), nil, nil)}
	attachContributionFixture(t, h, "alice")
	return h
}

func isDuplicateContribution(err error) bool {
	var policy *policyError
	return errors.As(err, &policy) && policy.Code == "account_duplicate"
}

func TestParseCodexClaimsIdentifiesWorkspaceMember(t *testing.T) {
	member := parseCodexClaims(codexTestIDToken(t, testCodexWorkspace, "user-a", "A@Example.com"))
	if member.ChatGPTAccountID != testCodexWorkspace || member.userKey() != "user-a" {
		t.Fatalf("claims=%+v", member)
	}
	if got, want := member.contributionIdentity(), testCodexWorkspace+"|user:user-a"; got != want {
		t.Fatalf("identity=%q want %q", got, want)
	}
	byEmail := parseCodexClaims(codexTestIDToken(t, testCodexWorkspace, "", "A@Example.com"))
	if byEmail.userKey() != "a@example.com" {
		t.Fatalf("email fallback=%q", byEmail.userKey())
	}
	anonymous := parseCodexClaims(codexTestIDToken(t, testCodexWorkspace, "", ""))
	if anonymous.userKey() != "" || anonymous.contributionIdentity() != testCodexWorkspace {
		t.Fatalf("anonymous identity=%q", anonymous.contributionIdentity())
	}
	if parseCodexClaims(codexTestIDToken(t, "", "user-a", "")).contributionIdentity() != "" {
		t.Fatal("identity without a workspace")
	}
}

func TestCodexContributionAcceptsEachWorkspaceMember(t *testing.T) {
	h := codexContributionHandler(t)
	r := contributionRequestActor(httptest.NewRequest("POST", "/", nil), "alice")
	first, err := h.saveCodexContribution(r, codexTestTokens(t, testCodexWorkspace, "user-a", "a@example.com"))
	if err != nil {
		t.Fatalf("first member: %v", err)
	}
	second, err := h.saveCodexContribution(r, codexTestTokens(t, testCodexWorkspace, "user-b", "b@example.com"))
	if err != nil {
		t.Fatalf("second member of the same workspace rejected: %v", err)
	}
	if first == second {
		t.Fatalf("members share account id %q", first)
	}
	if _, err := h.saveCodexContribution(r, codexTestTokens(t, testCodexWorkspace, "user-a", "a@example.com")); !isDuplicateContribution(err) {
		t.Fatalf("same member re-added: %v", err)
	}
	if n := len(h.pool.allAccounts()); n != 2 {
		t.Fatalf("accounts=%d", n)
	}
}

// Accounts registered before identities were user-scoped recorded only the
// workspace; they must still block their own user but not a teammate.
func TestCodexContributionHonorsWorkspaceOnlyRegistrations(t *testing.T) {
	h := codexContributionHandler(t)
	r := contributionRequestActor(httptest.NewRequest("POST", "/", nil), "alice")
	legacy := codexTestTokens(t, testCodexWorkspace, "user-a", "a@example.com")
	data, err := json.Marshal(map[string]any{"tokens": map[string]any{"id_token": legacy.IDToken, "access_token": legacy.AccessToken, "refresh_token": legacy.RefreshToken}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.persistContribution("alice", AccountTypeCodex, contributionIdentity{Value: testCodexWorkspace}, func(path string) error { return h.cfg.vault.writeAccountFile(path, data) }); err != nil {
		t.Fatalf("legacy registration: %v", err)
	}
	if _, err := h.saveCodexContribution(r, codexTestTokens(t, testCodexWorkspace, "user-a", "a@example.com")); !isDuplicateContribution(err) {
		t.Fatalf("legacy owner re-added under the new identity: %v", err)
	}
	if _, err := h.saveCodexContribution(r, codexTestTokens(t, testCodexWorkspace, "user-b", "b@example.com")); err != nil {
		t.Fatalf("teammate blocked by a workspace-only registration: %v", err)
	}
	if _, err := h.saveCodexContribution(r, codexTestTokens(t, "other-workspace", "user-a", "a@example.com")); err != nil {
		t.Fatalf("same user in another workspace: %v", err)
	}
}

func TestCodexContributionKeepsUnidentifiedRegistrationsExclusive(t *testing.T) {
	h := codexContributionHandler(t)
	r := contributionRequestActor(httptest.NewRequest("POST", "/", nil), "alice")
	tokens := codexTestTokens(t, testCodexWorkspace, "", "")
	data, err := json.Marshal(map[string]any{"tokens": map[string]any{"id_token": tokens.IDToken, "access_token": tokens.AccessToken, "refresh_token": "legacy"}})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.persistContribution("alice", AccountTypeCodex, contributionIdentity{Value: testCodexWorkspace}, func(path string) error { return h.cfg.vault.writeAccountFile(path, data) }); err != nil {
		t.Fatal(err)
	}
	if _, err := h.saveCodexContribution(r, codexTestTokens(t, testCodexWorkspace, "user-b", "b@example.com")); !isDuplicateContribution(err) {
		t.Fatalf("registration with no known user must stay exclusive: %v", err)
	}
}

func TestCodexReloginRejectsAnotherWorkspaceMember(t *testing.T) {
	dir := t.TempDir()
	original := codexTestTokens(t, testCodexWorkspace, "user-a", "a@example.com")
	file := filepath.Join(dir, "member.json")
	raw, err := json.Marshal(map[string]any{"tokens": map[string]any{"id_token": original.IDToken, "access_token": original.AccessToken, "refresh_token": original.RefreshToken}, "added_at": "2026-11-26T12:00:00Z"})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(file, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	account := &Account{Type: AccountTypeCodex, ID: "member", File: file, IDToken: original.IDToken, vault: plainTestVault()}
	account.codex.AccountID = testCodexWorkspace
	h := &proxyHandler{pool: newPoolState([]*Account{account}, false)}

	if err := h.replaceCodexAccountCredentials("member", codexTestTokens(t, testCodexWorkspace, "user-b", "b@example.com")); err == nil {
		t.Fatal("teammate login replaced the account's credentials")
	}
	if data, err := os.ReadFile(file); err != nil || !strings.Contains(string(data), original.RefreshToken) {
		t.Fatalf("credentials changed after rejected relogin: %v", err)
	}
	renewed := codexTestTokens(t, testCodexWorkspace, "user-a", "a@example.com")
	renewed.RefreshToken = "renewed-refresh"
	if err := h.replaceCodexAccountCredentials("member", renewed); err != nil {
		t.Fatalf("own relogin: %v", err)
	}
	if data, err := os.ReadFile(file); err != nil || !strings.Contains(string(data), "renewed-refresh") {
		t.Fatalf("own relogin did not persist: %v", err)
	}
}
