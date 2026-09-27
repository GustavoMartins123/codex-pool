package main

import (
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codex-pool-proxy/internal/credstore"
)

func useKeyedVault(t *testing.T, current credstore.Key, previous *credstore.Key) {
	t.Helper()
	store, err := credstore.NewKeyedStore(current, previous)
	if err != nil {
		t.Fatal(err)
	}
	previousStore := accountCredentialStore
	accountCredentialStore = store
	t.Cleanup(func() { accountCredentialStore = previousStore })
}

func hexKey(t *testing.T, version int, char byte) credstore.Key {
	t.Helper()
	key, err := credstore.ParseKey(version, strings.Repeat(string(char), 64))
	if err != nil {
		t.Fatal(err)
	}
	return key
}

func TestVaultRoundTripThroughAtomicWrite(t *testing.T) {
	useKeyedVault(t, hexKey(t, 1, 'a'), nil)
	dir := t.TempDir()
	path := filepath.Join(dir, "account.json")

	if err := atomicWriteJSON(path, map[string]any{"api_key": "super-secret"}); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !credstore.IsEncrypted(raw) {
		t.Fatalf("file on disk must be an encrypted envelope, got: %s", raw)
	}
	if strings.Contains(string(raw), "super-secret") {
		t.Fatal("plaintext secret leaked to disk")
	}
	back, err := readAccountFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(back), "super-secret") {
		t.Fatalf("decoded file lost the secret: %s", back)
	}
}

func TestLoadPoolDecodesEncryptedCredentials(t *testing.T) {
	poolDir := t.TempDir()
	codexDir := filepath.Join(poolDir, "codex")
	if err := os.MkdirAll(codexDir, 0o700); err != nil {
		t.Fatal(err)
	}
	plaintext := []byte(`{"tokens":{"access_token":"tok-123","account_id":"acct-1"},"email":"a@example.com"}`)

	useKeyedVault(t, hexKey(t, 1, 'b'), nil)
	atRest, err := accountCredentialStore.Encode(plaintext)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexDir, "one.json"), atRest, 0o600); err != nil {
		t.Fatal(err)
	}

	base, _ := url.Parse("https://chatgpt.com/backend-api/codex")
	claudeBase, _ := url.Parse("https://api.anthropic.com")
	geminiBase, _ := url.Parse("https://generativelanguage.googleapis.com")
	registry := NewProviderRegistry(NewCodexProvider(base, base, nil), NewClaudeProvider(claudeBase), NewGeminiProvider(geminiBase, geminiBase))
	accounts, err := loadPool(poolDir, registry)
	if err != nil {
		t.Fatal(err)
	}
	if len(accounts) != 1 {
		t.Fatalf("expected 1 account, got %d", len(accounts))
	}
	if accounts[0].AccessToken != "tok-123" || accounts[0].AccountID != "acct-1" {
		t.Fatalf("account not decoded correctly: %+v", accounts[0])
	}
}

func TestMigrateAndRollbackCredentialFiles(t *testing.T) {
	poolDir := t.TempDir()
	codexDir := filepath.Join(poolDir, "codex")
	if err := os.MkdirAll(codexDir, 0o700); err != nil {
		t.Fatal(err)
	}
	plain := filepath.Join(codexDir, "plain.json")
	if err := os.WriteFile(plain, []byte(`{"api_key":"k1"}`), 0o600); err != nil {
		t.Fatal(err)
	}

	// An old-version envelope (kv=1) plus a plaintext file.
	v1 := hexKey(t, 1, 'c')
	v2 := hexKey(t, 2, 'd')
	oldStore, err := credstore.NewKeyedStore(v1, nil)
	if err != nil {
		t.Fatal(err)
	}
	oldAtRest, err := oldStore.Encode([]byte(`{"api_key":"k2"}`))
	if err != nil {
		t.Fatal(err)
	}
	old := filepath.Join(codexDir, "old.json")
	if err := os.WriteFile(old, oldAtRest, 0o600); err != nil {
		t.Fatal(err)
	}

	useKeyedVault(t, v2, &v1)
	encrypted, rotated, err := migrateCredentialFiles(poolDir)
	if err != nil {
		t.Fatal(err)
	}
	if encrypted != 1 || rotated != 1 {
		t.Fatalf("migration counts = encrypted %d, rotated %d; want 1 and 1", encrypted, rotated)
	}
	for _, path := range []string{plain, old} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if !credstore.IsEncrypted(raw) {
			t.Fatalf("%s must be encrypted after migration", path)
		}
		if !strings.Contains(string(raw), `"kv":2`) {
			t.Fatalf("%s must be at key version 2", path)
		}
	}

	// Second run is a no-op.
	encrypted, rotated, err = migrateCredentialFiles(poolDir)
	if err != nil || encrypted != 0 || rotated != 0 {
		t.Fatalf("second migration must be a no-op, got encrypted=%d rotated=%d err=%v", encrypted, rotated, err)
	}

	// Rollback: decrypt everything back to plaintext.
	count, err := decryptCredentialFiles(poolDir)
	if err != nil {
		t.Fatal(err)
	}
	if count != 2 {
		t.Fatalf("decrypted %d files, want 2", count)
	}
	for _, path := range []string{plain, old} {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		if credstore.IsEncrypted(raw) || !strings.Contains(string(raw), `"api_key"`) {
			t.Fatalf("%s must be plaintext after rollback: %s", path, raw)
		}
	}
}

func TestEncryptedFilesWithoutKeyFailStartup(t *testing.T) {
	poolDir := t.TempDir()
	codexDir := filepath.Join(poolDir, "codex")
	if err := os.MkdirAll(codexDir, 0o700); err != nil {
		t.Fatal(err)
	}

	useKeyedVault(t, hexKey(t, 1, 'e'), nil)
	atRest, err := accountCredentialStore.Encode([]byte(`{"api_key":"k"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexDir, "one.json"), atRest, 0o600); err != nil {
		t.Fatal(err)
	}

	// Simulate a restart without the key configured.
	accountCredentialStore = credstore.PlainStore{}
	t.Cleanup(func() { accountCredentialStore = credstore.PlainStore{} })
	if err := ensureNoEncryptedFilesWithoutKey(poolDir); err == nil {
		t.Fatal("encrypted files without a key must fail startup")
	}
	if err := ensureNoEncryptedFilesWithoutKey(t.TempDir()); err != nil {
		t.Fatalf("empty pool dir must not fail: %v", err)
	}
}

func TestBuildCredentialStoreFromEnv(t *testing.T) {
	t.Setenv("POOL_CREDENTIAL_KEY", "")
	os.Unsetenv("POOL_CREDENTIAL_KEY")
	store, err := buildCredentialStore()
	if err != nil || store.Enabled() {
		t.Fatalf("no key must yield the plain store, got enabled=%v err=%v", store.Enabled(), err)
	}

	t.Setenv("POOL_CREDENTIAL_KEY", strings.Repeat("f", 64))
	t.Setenv("POOL_CREDENTIAL_KEY_VERSION", "3")
	t.Setenv("POOL_CREDENTIAL_KEY_PREVIOUS", strings.Repeat("e", 64))
	store, err = buildCredentialStore()
	if err != nil {
		t.Fatal(err)
	}
	keyed, ok := store.(*credstore.KeyedStore)
	if !ok || !store.Enabled() {
		t.Fatalf("expected keyed store, got %T", store)
	}
	if keyed.CurrentVersion() != 3 {
		t.Fatalf("current version = %d, want 3", keyed.CurrentVersion())
	}

	t.Setenv("POOL_CREDENTIAL_KEY", "short")
	if _, err := buildCredentialStore(); err == nil {
		t.Fatal("weak key material must be rejected")
	}
}
