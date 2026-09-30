package main

import (
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"codex-pool-proxy/internal/credstore"
)

func setupFixture(t *testing.T) (string, string) {
	t.Helper()
	dir := filepath.Join(t.TempDir(), "setup fixture")
	if err := os.Mkdir(dir, 0700); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{setupScriptName, ".env.example"} {
		data, err := os.ReadFile(name)
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return dir, filepath.Join(dir, ".env")
}

func runSetup(t *testing.T, dir string, args ...string) (string, error) {
	t.Helper()
	cmd := setupTestCommand(filepath.Join(dir, setupScriptName), args...)
	cmd.Dir = dir
	output, err := cmd.CombinedOutput()
	return string(output), err
}

func readSetupEnv(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

func TestSetupGeneratesValidSecretsWithoutPrintingThem(t *testing.T) {
	dir, envPath := setupFixture(t)
	output, err := runSetup(t, dir)
	if err != nil {
		t.Fatalf("setup failed: %v (%s)", err, output)
	}
	content := readSetupEnv(t, envPath)
	keys := []string{"ADMIN_TOKEN", "POOL_AUTH_ENCRYPTION_KEY", "POOL_JWT_SECRET", "POOL_CREDENTIAL_KEY"}
	seen := map[string]bool{}
	for _, key := range keys {
		matches := regexp.MustCompile(`(?m)^` + key + `=([0-9a-f]{64})\r?$`).FindStringSubmatch(content)
		if len(matches) != 2 {
			t.Fatalf("missing or invalid %s", key)
		}
		if seen[matches[1]] {
			t.Fatal("generated keys are not independent")
		}
		seen[matches[1]] = true
		if strings.Contains(output, matches[1]) {
			t.Fatalf("%s was printed", key)
		}
		if _, err := credstore.ParseKey(1, matches[1]); err != nil {
			t.Fatalf("generated %s rejected", key)
		}
	}
	if output, err := runSetup(t, dir); err != nil {
		t.Fatalf("rerun failed: %v (%s)", err, output)
	}
	if readSetupEnv(t, envPath) != content {
		t.Fatal("rerun changed existing environment")
	}
	checkSetupPermissions(t, envPath)
	if _, err := os.Stat(envPath + ".setup-lock"); !os.IsNotExist(err) {
		t.Fatal("setup left its lock behind")
	}
	leftovers, err := filepath.Glob(envPath + ".tmp.*")
	if err != nil || len(leftovers) != 0 {
		t.Fatal("setup left temporary secrets behind")
	}
}

func TestSetupPreservesKeysRotationAndProviderSettings(t *testing.T) {
	dir, _ := setupFixture(t)
	envPath := filepath.Join(dir, "custom env")
	content := "# private settings\r\nexport ADMIN_TOKEN='existing-admin-token' # keep\r\nPOOL_AUTH_ENCRYPTION_KEY=\r\nPOOL_JWT_SECRET=\"existing-client-signing-secret#with-more-than-32-chars\" # keep\r\nPOOL_CREDENTIAL_KEY=\r\nPOOL_AUTH_ENCRYPTION_KEY_OLD=keep-old-sealing-key\r\nPOOL_CREDENTIAL_KEY_PREVIOUS=keep-old-vault-key\r\nPOOL_CREDENTIAL_KEY_VERSION=7\r\nGEMINI_OAUTH_CLIENT_SECRET=provider-issued-secret\r\nPUBLIC_URL=https://pool.example.com\r\n"
	if err := os.WriteFile(envPath, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
	output, err := runSetup(t, dir, envPath)
	if err != nil {
		t.Fatalf("setup failed: %v (%s)", err, output)
	}
	after := readSetupEnv(t, envPath)
	for _, line := range strings.Split(strings.ReplaceAll(content, "\r", ""), "\n") {
		if line == "" || strings.HasSuffix(line, "=") {
			continue
		}
		if !strings.Contains(after, line) {
			t.Fatal("setup changed an existing setting")
		}
	}
	if output, err := runSetup(t, dir, envPath); err != nil {
		t.Fatalf("rerun failed: %v (%s)", err, output)
	}
	if readSetupEnv(t, envPath) != after {
		t.Fatal("rerun rotated a key")
	}
	if _, err := os.Stat(filepath.Join(dir, ".env")); !os.IsNotExist(err) {
		t.Fatal("custom path also changed the default environment")
	}
	checkSetupPermissions(t, envPath)
}

func TestSetupRefusesInvalidConfigurationWithoutWriting(t *testing.T) {
	cases := map[string]string{
		"short":             "POOL_JWT_SECRET=short\n",
		"placeholder":       "POOL_CREDENTIAL_KEY=<replace-this-with-your-secret-key>\n",
		"admin-placeholder": "ADMIN_TOKEN=your-secret-here\n",
		"nul-byte":          "# invalid\x00file\n",
		"interpolation":     "POOL_AUTH_ENCRYPTION_KEY=${DO_NOT_REPLACE_EXISTING_SECRET}\n",
		"duplicates":        "ADMIN_TOKEN=\nexport ADMIN_TOKEN=\n",
		"malformed":         "POOL_CREDENTIAL_KEY: missing-equals\n",
		"unmatched-quote":   "POOL_CREDENTIAL_KEY=\"existing-secret-with-at-least-32-characters\n",
	}
	for name, content := range cases {
		t.Run(name, func(t *testing.T) {
			dir, envPath := setupFixture(t)
			if err := os.WriteFile(envPath, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := runSetup(t, dir); err == nil {
				t.Fatal("invalid configuration accepted")
			}
			if readSetupEnv(t, envPath) != content {
				t.Fatal("failed setup changed environment")
			}
			if _, err := os.Stat(envPath + ".setup-lock"); !os.IsNotExist(err) {
				t.Fatal("failed setup left its lock behind")
			}
		})
	}
}

func TestSetupRefusesAnExistingLock(t *testing.T) {
	dir, envPath := setupFixture(t)
	if err := os.Mkdir(envPath+".setup-lock", 0700); err != nil {
		t.Fatal(err)
	}
	if _, err := runSetup(t, dir); err == nil {
		t.Fatal("concurrent setup accepted")
	}
	if _, err := os.Stat(envPath); !os.IsNotExist(err) {
		t.Fatal("locked setup created environment")
	}
	if _, err := os.Stat(envPath + ".setup-lock"); err != nil {
		t.Fatal("setup removed another invocation's lock")
	}
}

func TestSetupRefusesMissingTemplate(t *testing.T) {
	dir, envPath := setupFixture(t)
	if err := os.Remove(filepath.Join(dir, ".env.example")); err != nil {
		t.Fatal(err)
	}
	if _, err := runSetup(t, dir); err == nil {
		t.Fatal("missing template accepted")
	}
	if _, err := os.Stat(envPath); !os.IsNotExist(err) {
		t.Fatal("failed setup created environment")
	}
}
