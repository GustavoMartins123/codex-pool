package main

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"codex-pool-proxy/internal/credstore"
)

// P1-05: the vault is mandatory — no key, no startup, regardless of pool state.
func TestInitCredentialVaultRequiresKey(t *testing.T) {
	t.Setenv("POOL_CREDENTIAL_KEY", "")
	os.Unsetenv("POOL_CREDENTIAL_KEY")
	t.Setenv("POOL_CREDENTIAL_KEY_PREVIOUS", "")
	os.Unsetenv("POOL_CREDENTIAL_KEY_PREVIOUS")
	previous := accountCredentialStore
	t.Cleanup(func() { accountCredentialStore = previous })

	for _, pool := range []string{t.TempDir(), t.TempDir()} {
		if pool == "" {
			t.Fatal("unreachable")
		}
		err := initCredentialVault(pool)
		if err == nil || !strings.Contains(err.Error(), "POOL_CREDENTIAL_KEY is required") {
			t.Fatalf("startup without key must be refused, got: %v", err)
		}
	}
}

// P1-05: legacy plaintext only ever enters through the startup migrator, and
// afterwards no plaintext remains on disk.
func TestInitCredentialVaultMigratesPlaintextPool(t *testing.T) {
	t.Setenv("POOL_CREDENTIAL_KEY", strings.Repeat("ab", 32))
	t.Setenv("POOL_CREDENTIAL_KEY_VERSION", "1")
	poolDir := t.TempDir()
	codexDir := filepath.Join(poolDir, "codex")
	if err := os.MkdirAll(codexDir, 0o700); err != nil {
		t.Fatal(err)
	}
	plaintext := []byte(`{"tokens":{"access_token":"legacy"}}`)
	if err := os.WriteFile(filepath.Join(codexDir, "one.json"), plaintext, 0o600); err != nil {
		t.Fatal(err)
	}

	previous := accountCredentialStore
	t.Cleanup(func() { accountCredentialStore = previous })
	if err := initCredentialVault(poolDir); err != nil {
		t.Fatal(err)
	}
	if !accountCredentialStore.Enabled() {
		t.Fatal("vault not active after init")
	}
	raw, err := os.ReadFile(filepath.Join(codexDir, "one.json"))
	if err != nil {
		t.Fatal(err)
	}
	if !credstore.IsEncrypted(raw) {
		t.Fatal("plaintext survived startup migration")
	}
	decoded, err := accountCredentialStore.Decode(raw)
	if err != nil || string(decoded) != string(plaintext) {
		t.Fatalf("migrated file does not decode to the original payload: %v", err)
	}

	checked, failures := verifyCredentialFiles(poolDir)
	if checked != 1 || len(failures) != 0 {
		t.Fatalf("verify after migration: checked=%d failures=%v", checked, failures)
	}
}

func TestVerifyCredentialFilesFlagsUndecodable(t *testing.T) {
	poolDir := t.TempDir()
	codexDir := filepath.Join(poolDir, "codex")
	if err := os.MkdirAll(codexDir, 0o700); err != nil {
		t.Fatal(err)
	}
	useKeyedVault(t, hexKey(t, 1, 'c'), nil)
	atRest, err := accountCredentialStore.Encode([]byte(`{"api_key":"good"}`))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexDir, "good.json"), atRest, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(codexDir, "bad.json"), []byte(`{"envelope":{"kv":99}}`), 0o600); err != nil {
		t.Fatal(err)
	}

	checked, failures := verifyCredentialFiles(poolDir)
	if checked != 2 || len(failures) != 1 || !strings.Contains(failures[0], "bad.json") {
		t.Fatalf("checked=%d failures=%v", checked, failures)
	}
}
