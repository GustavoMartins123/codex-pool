package main

import (
	"encoding/json"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codex-pool-proxy/internal/credstore"
)

// CP-02 invariant: with a master key configured, no code path may leave an
// upstream credential in plaintext in the pool directory. The regression that
// motivated this test was the Codex re-login and Z.ai OAuth paths writing
// raw JSON through os.WriteFile/os.OpenFile while the vault was enabled.
func TestInvariantNoCredentialFileBypassesTheVault(t *testing.T) {
	useKeyedVault(t, hexKey(t, 1, 'c'), nil)
	dir := t.TempDir()

	codexFile := filepath.Join(dir, "codex-relogin.json")
	plain := []byte(`{"tokens":{"id_token":"old-id","access_token":"old-access","refresh_token":"old-refresh"}}`)
	if err := os.WriteFile(codexFile, plain, 0o600); err != nil {
		t.Fatal(err)
	}
	// The file must start out encrypted too, otherwise the test would pass
	// trivially for the wrong reason.
	if err := writeAccountFile(codexFile, plain); err != nil {
		t.Fatal(err)
	}

	acc := &Account{ID: "codex-relogin", Type: AccountTypeCodex, File: codexFile, AccountID: "acct-1"}
	h := &proxyHandler{pool: newPoolState([]*Account{acc}, false)}
	if err := h.replaceCodexAccountCredentials("codex-relogin", &CodexTokenResponse{
		IDToken:      "new-id",
		AccessToken:  "new-access",
		RefreshToken: "new-refresh",
	}); err != nil {
		t.Fatalf("codex re-login failed: %v", err)
	}
	assertEncryptedCredentialFile(t, codexFile, "new-access", "new-refresh")

	zaiDir := filepath.Join(dir, "zai")
	base, _ := url.Parse("https://provider.example")
	zaiHandler := &proxyHandler{cfg: &config{poolDir: dir}, registry: NewProviderRegistry(nil, nil, nil, NewZAIProvider(base)), pool: newPoolState(nil, false)}
	attachContributionFixture(t, zaiHandler, "fixture")
	if _, err := zaiHandler.saveContribution(contributionRequestActor(httptest.NewRequest("POST", "/", nil), "fixture"), AccountTypeZAI, "user-1", ZAIAuthJSON{APIKey: "zai-api-key", ZCodeJWT: "zai-jwt", BusinessToken: "zai-business-token"}); err != nil {
		t.Fatal(err)
	}
	entries, err := os.ReadDir(zaiDir)
	if err != nil {
		t.Fatalf("zai account file was not written: %v", err)
	}
	if len(entries) != 1 {
		t.Fatalf("expected 1 zai account file, got %d", len(entries))
	}
	assertEncryptedCredentialFile(t, filepath.Join(zaiDir, entries[0].Name()), "zai-api-key", "zai-jwt", "zai-business-token")
}

// TestCredentialWritersGoThroughTheVault pins every credential write helper to
// the vault: a new writer that forgets to encode must fail here.
func TestCredentialWritersGoThroughTheVault(t *testing.T) {
	useKeyedVault(t, hexKey(t, 1, 'q'), nil)
	dir := t.TempDir()

	// atomicWriteJSON (used by saveAccount, claude and antigravity refreshes).
	jsonPath := filepath.Join(dir, "atomic.json")
	if err := atomicWriteJSON(jsonPath, map[string]any{"api_key": "atomic-secret"}); err != nil {
		t.Fatal(err)
	}
	assertEncryptedCredentialFile(t, jsonPath, "atomic-secret")

	// writeAccountFile (used by the admin add/relogin handlers).
	rawPath := filepath.Join(dir, "raw.json")
	if err := writeAccountFile(rawPath, []byte(`{"api_key":"raw-secret"}`)); err != nil {
		t.Fatal(err)
	}
	assertEncryptedCredentialFile(t, rawPath, "raw-secret")

	// Migration and rollback must also stay at rest / return to plaintext.
	plainPath := filepath.Join(dir, "plain.json")
	if err := os.WriteFile(plainPath, []byte(`{"api_key":"migrate-secret"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := migrateCredentialFiles(dir); err != nil {
		t.Fatal(err)
	}
	assertEncryptedCredentialFile(t, plainPath, "migrate-secret")
}

func assertEncryptedCredentialFile(t *testing.T, path string, forbidden ...string) {
	t.Helper()
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read %s: %v", path, err)
	}
	if !credstore.IsEncrypted(raw) {
		t.Fatalf("%s is not an encrypted envelope: %s", path, raw)
	}
	for _, secret := range forbidden {
		if strings.Contains(string(raw), secret) {
			t.Fatalf("%s leaked %q to disk", path, secret)
		}
	}
	decoded, err := readAccountFile(path)
	if err != nil {
		t.Fatalf("readAccountFile(%s): %v", path, err)
	}
	if !json.Valid(decoded) {
		t.Fatalf("%s did not decode back to JSON: %s", path, decoded)
	}
	for _, secret := range forbidden {
		if !strings.Contains(string(decoded), secret) {
			t.Fatalf("%s lost %q after decode: %s", path, secret, decoded)
		}
	}
}
