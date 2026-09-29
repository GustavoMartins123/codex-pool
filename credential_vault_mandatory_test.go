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

	stats, err := verifyCredentialFiles(poolDir)
	if err != nil || stats.Checked != 1 || stats.Current != 1 || len(stats.Failures) != 0 {
		t.Fatalf("verify after migration: %+v err=%v", stats, err)
	}
}

func TestCredentialVaultRejectsExamplePlaceholder(t *testing.T) {
	placeholders := []string{
		"<64-hex-chars-or-long-passphrase>",
		"changeme",
		"change-me",
		"example",
		"your-secret-here",
		"  <generate-with-openssl-rand-hex-32>  ",
	}
	for _, raw := range placeholders {
		t.Setenv("POOL_CREDENTIAL_KEY", raw)
		t.Setenv("POOL_CREDENTIAL_KEY_VERSION", "")
		os.Unsetenv("POOL_CREDENTIAL_KEY_VERSION")
		previous := accountCredentialStore
		t.Cleanup(func() { accountCredentialStore = previous })
		if err := initCredentialVault(t.TempDir()); err == nil {
			t.Fatalf("placeholder %q must be rejected", raw)
		}
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

	stats, err := verifyCredentialFiles(poolDir)
	if err != nil {
		t.Fatal(err)
	}
	if stats.Checked != 2 || stats.Current != 1 || stats.Plaintext != 1 || len(stats.Failures) != 1 || !strings.Contains(stats.Failures[0], "bad.json") {
		t.Fatalf("stats=%+v failures=%v", stats, stats.Failures)
	}
}

func TestVerifyCredentialFilesFailsOnMissingPoolDir(t *testing.T) {
	useKeyedVault(t, hexKey(t, 1, 'c'), nil)
	_, err := verifyCredentialFiles(filepath.Join(t.TempDir(), "does-not-exist"))
	if err == nil {
		t.Fatal("walk errors must fail the check instead of returning a clean pass")
	}
}
