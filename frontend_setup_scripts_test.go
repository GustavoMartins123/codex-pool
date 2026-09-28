package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestServeCodexSetupScript_PowerShell(t *testing.T) {
	h := &proxyHandler{}

	req := httptest.NewRequest(http.MethodGet, "http://example.com/setup/codex/testtoken?shell=powershell", nil)
	rr := httptest.NewRecorder()
	h.serveCodexSetupScript(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type = %q, want text/plain*", ct)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "Set-StrictMode -Version Latest") {
		t.Fatalf("expected PowerShell script body, got:\n%s", body)
	}
	if !strings.Contains(body, "Join-Path $HOME '.codex'") {
		t.Fatalf("expected codex paths in script body, got:\n%s", body)
	}
	if !strings.Contains(body, "model_catalog_json = ") {
		t.Fatalf("expected model catalog config in script body, got:\n%s", body)
	}
	for _, setting := range []string{
		"experimental_realtime_webrtc_call_base_url",
		"experimental_realtime_ws_base_url",
		`realtime.version = "v3"`,
		`realtime.transport = "webrtc"`,
	} {
		if !strings.Contains(body, setting) {
			t.Fatalf("expected %s in PowerShell setup script", setting)
		}
	}
	if !strings.Contains(body, "[mcp_servers.model_sync]") {
		t.Fatalf("expected MCP sidecar config in script body, got:\n%s", body)
	}
	if !strings.Contains(body, "model_sync.ps1") {
		t.Fatalf("expected MCP sidecar script install in PowerShell body, got:\n%s", body)
	}
	if !strings.Contains(body, "$firstLine = [Console]::In.ReadLine()") {
		t.Fatalf("expected MCP JSONL transport support in PowerShell body, got:\n%s", body)
	}
	if !strings.Contains(body, "features enable realtime_conversation") {
		t.Fatal("expected PowerShell setup to enable realtime_conversation")
	}
}

func TestServeCodexSetupScript_Bash(t *testing.T) {
	h := &proxyHandler{}

	req := httptest.NewRequest(http.MethodGet, "http://example.com/setup/codex/testtoken", nil)
	rr := httptest.NewRecorder()
	h.serveCodexSetupScript(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/x-shellscript") {
		t.Fatalf("Content-Type = %q, want text/x-shellscript*", ct)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "model_sync.sh") {
		t.Fatalf("expected MCP sidecar script install in bash body, got:\n%s", body)
	}
	if !strings.Contains(body, "model_catalog_json = ") {
		t.Fatalf("expected model catalog config in bash script body, got:\n%s", body)
	}
	for _, setting := range []string{
		"experimental_realtime_webrtc_call_base_url",
		"experimental_realtime_ws_base_url",
		`realtime.version = "v3"`,
		`realtime.transport = "webrtc"`,
	} {
		if !strings.Contains(body, setting) {
			t.Fatalf("expected %s in bash setup script", setting)
		}
	}
	if !strings.Contains(body, "[mcp_servers.model_sync]") {
		t.Fatalf("expected MCP sidecar config in bash script body, got:\n%s", body)
	}
	if !strings.Contains(body, "MCP_TRANSPORT_MODE=\"jsonl\"") {
		t.Fatalf("expected MCP JSONL transport support in bash body, got:\n%s", body)
	}
	if !strings.Contains(body, "features enable realtime_conversation") {
		t.Fatal("expected bash setup to enable realtime_conversation")
	}
}

func TestCodexSetupDefaultModel(t *testing.T) {
	h := &proxyHandler{}
	for _, shell := range []string{"bash", "powershell"} {
		rr := httptest.NewRecorder()
		h.serveCodexSetupScript(rr, httptest.NewRequest(http.MethodGet, "http://example.com/setup/codex/token?shell="+shell, nil))
		if !strings.Contains(rr.Body.String(), `model = "gpt-6-astra"`) {
			t.Fatalf("%s setup is missing the Astra default", shell)
		}
	}
}

func TestCodexSetupPreservesModel(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash script semantics are verified on Linux and CI")
	}
	h := &proxyHandler{}
	rr := httptest.NewRecorder()
	h.serveCodexSetupScript(rr, httptest.NewRequest(http.MethodGet, "http://example.com/setup/codex/token", nil))
	body := rr.Body.String()
	start := strings.Index(body, `echo "4. Updating configuration..."`)
	end := strings.Index(body, "if command -v codex >/dev/null")
	if start < 0 || end <= start {
		t.Fatal("configuration section missing")
	}

	for _, tc := range []struct{ name, initial, want string }{
		{"fresh", "", "gpt-6-astra"},
		{"selected", "model = \"gpt-5.6-sol\"\n", "gpt-5.6-sol"},
		{"pool update", "model_provider = \"codex-pool\"\n", "gpt-6-astra"},
		{"pool selection", "model = \"gpt-5.6-luna\"\nmodel_provider = \"codex-pool\"\n", "gpt-5.6-luna"},
		{"profile only", "[profiles.fast]\nmodel = \"gpt-5.6-luna\"\n", "gpt-6-astra"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			configFile := filepath.Join(dir, "config.toml")
			if err := os.WriteFile(configFile, []byte(tc.initial), 0o600); err != nil {
				t.Fatal(err)
			}
			for range 2 {
				cmd := exec.Command("bash", "-eu")
				cmd.Stdin = strings.NewReader(body[start:end])
				cmd.Env = append(os.Environ(), "CONFIG_FILE="+configFile, "BASE_URL=http://example.com", "MODEL_CATALOG="+filepath.Join(dir, "models.json"), "MCP_SCRIPT="+filepath.Join(dir, "sync.sh"))
				if output, err := cmd.CombinedOutput(); err != nil {
					t.Fatalf("configure: %v\n%s", err, output)
				}
			}
			data, err := os.ReadFile(configFile)
			if err != nil {
				t.Fatal(err)
			}
			root := strings.SplitN(string(data), "[", 2)[0]
			if !strings.Contains(root, `model = "`+tc.want+`"`) || strings.Count(root, "model =") != 1 {
				t.Fatalf("expected one root model %q:\n%s", tc.want, data)
			}
			if tc.initial != "" && !strings.Contains(string(data), tc.initial) {
				t.Fatalf("existing configuration changed:\n%s", data)
			}
		})
	}
}

func TestCodexSetupInstallsRestoreHelper(t *testing.T) {
	h := &proxyHandler{}
	for _, tc := range []struct{ shell, helper, backup string }{
		{"bash", "restore-config.sh", "config.toml.bak.$STAMP"},
		{"powershell", "restore-config.ps1", "config.toml.bak.$Stamp"},
	} {
		rr := httptest.NewRecorder()
		h.serveCodexSetupScript(rr, httptest.NewRequest(http.MethodGet, "http://example.com/setup/codex/token?shell="+tc.shell, nil))
		body := rr.Body.String()
		if !strings.Contains(body, tc.helper) {
			t.Fatalf("%s setup does not install %s", tc.shell, tc.helper)
		}
		if !strings.Contains(body, tc.backup) {
			t.Fatalf("%s setup does not reference the versioned backup %q", tc.shell, tc.backup)
		}
	}
}

func TestCodexSetupRefreshesExistingPoolConfig(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash script semantics are verified on Linux and CI")
	}
	h := &proxyHandler{}
	rr := httptest.NewRecorder()
	h.serveCodexSetupScript(rr, httptest.NewRequest(http.MethodGet, "http://example.com/setup/codex/token", nil))
	body := rr.Body.String()
	start := strings.Index(body, `echo "4. Updating configuration..."`)
	end := strings.Index(body, "if command -v codex >/dev/null")
	if start < 0 || end <= start {
		t.Fatal("configuration section missing")
	}

	dir := t.TempDir()
	configFile := filepath.Join(dir, "config.toml")
	syncScript := filepath.Join(dir, "sync.sh")
	initial := `model = "gpt-5.6-luna"
model_provider = "codex-pool"
chatgpt_base_url = "http://old.example/backend-api"

[model_providers.codex-pool]
name = "OpenAI via codex-pool proxy"
base_url = "http://old.example"
wire_api = "responses"

[mcp_servers.model_sync]
command = "bash"
args = ["/old/sync.sh", "http://old.example"]

[projects."/home/user/demo"]
trust_level = "trusted"
`
	if err := os.WriteFile(configFile, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}

	run := func() {
		cmd := exec.Command("bash", "-eu")
		cmd.Stdin = strings.NewReader(body[start:end])
		cmd.Env = append(os.Environ(), "CONFIG_FILE="+configFile, "BASE_URL=http://new.example", "MODEL_CATALOG="+filepath.Join(dir, "models.json"), "MCP_SCRIPT="+syncScript)
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("configure: %v\n%s", err, output)
		}
	}
	run()

	updated, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	text := string(updated)
	for _, want := range []string{
		`base_url = "http://new.example"`,
		`chatgpt_base_url = "http://new.example/backend-api"`,
		`args = ["` + syncScript + `", "http://new.example"]`,
		`[projects."/home/user/demo"]`,
		`model = "gpt-5.6-luna"`,
	} {
		if !strings.Contains(text, want) {
			t.Fatalf("expected %q after refresh:\n%s", want, text)
		}
	}
	if strings.Contains(text, "old.example") {
		t.Fatalf("stale pool URL survived refresh:\n%s", text)
	}
	if got := strings.Count(text, "[model_providers.codex-pool]"); got != 1 {
		t.Fatalf("expected one provider block after refresh, got %d:\n%s", got, text)
	}

	backups, err := filepath.Glob(configFile + ".bak.*")
	if err != nil || len(backups) != 1 {
		t.Fatalf("expected exactly one versioned backup, got %v (err=%v)", backups, err)
	}
	backupData, err := os.ReadFile(backups[0])
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(backupData), "http://old.example") {
		t.Fatalf("backup does not hold the previous configuration:\n%s", backupData)
	}

	run()
	rerun, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Count(string(rerun), "[model_providers.codex-pool]"); got != 1 {
		t.Fatalf("expected refresh to stay idempotent, got %d provider blocks:\n%s", got, rerun)
	}
}

func TestSetupExamplesUseAstra(t *testing.T) {
	for _, path := range []string{"web/src/App.tsx"} {
		data, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		body := string(data)
		if !strings.Contains(body, "gpt-6-astra") {
			t.Errorf("%s has no Astra onboarding example", path)
		}
		for _, stale := range []string{`model="gpt-5.6-luna"`, `model="gpt-5.6-sol"`, `model: "gpt-5.6-sol"`, `"model":"gpt-5.6-sol"`, "cute-code --model gpt-5.6-sol", "Use gpt-5.6-luna as the first smoke-test model"} {
			if strings.Contains(body, stale) {
				t.Errorf("%s retains stale onboarding example %q", path, stale)
			}
		}
	}
}

func TestServeGrokSetupScript_Bash(t *testing.T) {
	h := &proxyHandler{}
	req := httptest.NewRequest(http.MethodGet, "http://example.com/setup/grok/testtoken", nil)
	rr := httptest.NewRecorder()
	h.serveGrokSetupScript(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	body := rr.Body.String()
	for _, want := range []string{"[endpoints]", `models_base_url = \"`, `[model."%s"]`, "grok-4.5", "gpt-5.6-luna", "claude-sonnet-5", "auth.json.before-codex-pool", "/config/grok/$TOKEN"} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected Grok setup script to contain %q", want)
		}
	}
}

func TestServeGrokSetupScript_PowerShell(t *testing.T) {
	h := &proxyHandler{}
	req := httptest.NewRequest(http.MethodGet, "http://example.com/setup/grok/testtoken?shell=powershell", nil)
	rr := httptest.NewRecorder()
	h.serveGrokSetupScript(rr, req)

	if rr.Code != http.StatusOK || !strings.Contains(rr.Body.String(), "models_base_url") || !strings.Contains(rr.Body.String(), "[model.\"' + $Model.Id + '\"]") {
		t.Fatalf("PowerShell Grok setup missing proxy endpoint or model credentials: status=%d", rr.Code)
	}
}

func TestServeGrokSetupScript_BashPreservesConfigAndIsIdempotent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash script semantics are verified on Linux and CI")
	}
	h := &proxyHandler{}
	req := httptest.NewRequest(http.MethodGet, "http://example.com/setup/grok/testtoken", nil)
	rr := httptest.NewRecorder()
	h.serveGrokSetupScript(rr, req)

	home := t.TempDir()
	configDir := filepath.Join(home, ".grok")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	configFile := filepath.Join(configDir, "config.toml")
	initial := "[cli]\nauto_update = true\n\n[models]\ndefault = \"grok-4.5\"\n"
	if err := os.WriteFile(configFile, []byte(initial), 0o600); err != nil {
		t.Fatal(err)
	}
	authFile := filepath.Join(configDir, "auth.json")
	if err := os.WriteFile(authFile, []byte(`{"oauth":"credential"}`), 0o600); err != nil {
		t.Fatal(err)
	}
	binDir := filepath.Join(home, "bin")
	if err := os.MkdirAll(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	fakeCurl := "#!/bin/sh\nprintf '%s\\n' '{\"api_key\":\"pool-jwt\"}'\n"
	if err := os.WriteFile(filepath.Join(binDir, "curl"), []byte(fakeCurl), 0o700); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		cmd := exec.Command("bash")
		cmd.Stdin = strings.NewReader(rr.Body.String())
		cmd.Env = append(os.Environ(), "HOME="+home, "PATH="+binDir+":"+os.Getenv("PATH"))
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("run installer: %v\n%s", err, output)
		}
	}

	data, err := os.ReadFile(configFile)
	if err != nil {
		t.Fatal(err)
	}
	config := string(data)
	for _, want := range []string{"[cli]", "auto_update = true", `default = "grok-4.5"`, `[endpoints]`, `models_base_url = "http://example.com/v1"`, `api_key = "pool-jwt"`} {
		if !strings.Contains(config, want) {
			t.Fatalf("installed config missing %q:\n%s", want, config)
		}
	}
	if strings.Contains(config, "codex-pool-grok") {
		t.Fatalf("installer must not create or select a synthetic model:\n%s", config)
	}
	if count := strings.Count(config, `[model."grok-4.5"]`); count != 1 {
		t.Fatalf("grok-4.5 credential override count = %d, want 1:\n%s", count, config)
	}
	if _, err := os.Stat(authFile); !os.IsNotExist(err) {
		t.Fatalf("active Grok OAuth file still exists: %v", err)
	}
	if _, err := os.Stat(filepath.Join(configDir, "auth.json.before-codex-pool")); err != nil {
		t.Fatalf("Grok OAuth backup missing: %v", err)
	}
}

func TestServePiSetupScriptMergesProviders(t *testing.T) {
	h := &proxyHandler{}
	for _, target := range []string{
		"http://example.com/setup/pi/testtoken",
		"http://example.com/setup/pi/testtoken?shell=powershell",
	} {
		req := httptest.NewRequest(http.MethodGet, target, nil)
		rr := httptest.NewRecorder()
		h.servePiSetupScript(rr, req)
		if rr.Code != http.StatusOK {
			t.Fatalf("%s status = %d", target, rr.Code)
		}
		body := rr.Body.String()
		if !strings.Contains(body, "/config/pi/testtoken") || !strings.Contains(body, "providers") {
			t.Fatalf("%s did not generate a merging Pi installer", target)
		}
	}
}

func newSetupScriptHandler(t *testing.T) (*proxyHandler, string) {
	t.Helper()
	t.Setenv("POOL_AUTH_ENCRYPTION_KEY", "test-passport-encryption-key")
	passport, err := newPassportStore(testUsageStore(t).db)
	if err != nil {
		t.Fatalf("newPassportStore: %v", err)
	}
	_, _, client, _, err := passport.createGuest("operator", "setup tester", "S", nil)
	if err != nil {
		t.Fatalf("createGuest: %v", err)
	}
	nonce, _, err := passport.mintConfigDownloadNonce(client)
	if err != nil {
		t.Fatalf("mintConfigDownloadNonce: %v", err)
	}
	return &proxyHandler{cfg: &config{}, passport: passport}, nonce
}

func TestServeGeminiSetupScript_PowerShell(t *testing.T) {
	secret := "test-secret-key-12345678901234567890"
	t.Setenv("POOL_JWT_SECRET", secret)

	h, nonce := newSetupScriptHandler(t)

	req := httptest.NewRequest(http.MethodGet, "http://example.com/setup/gemini/"+nonce+"?shell=powershell", nil)
	rr := httptest.NewRecorder()
	h.serveGeminiSetupScript(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type = %q, want text/plain*", ct)
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store (script embeds a credential)", cc)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "$env:CODE_ASSIST_ENDPOINT = $BaseUrl") {
		t.Fatalf("expected PowerShell env setup in body, got:\n%s", body)
	}
	if strings.Contains(body, "`") {
		t.Fatalf("PowerShell script should not contain backticks (Go raw string safety), got:\n%s", body)
	}

	legacy := httptest.NewRequest(http.MethodGet, "http://example.com/setup/gemini/old-legacy-token", nil)
	legacyRR := httptest.NewRecorder()
	h.serveGeminiSetupScript(legacyRR, legacy)
	if legacyRR.Code != http.StatusNotFound {
		t.Fatalf("legacy token status = %d, want 404", legacyRR.Code)
	}

	// The Gemini script embeds the credential, so its redemption is single-use.
	replay := httptest.NewRequest(http.MethodGet, "http://example.com/setup/gemini/"+nonce, nil)
	replayRR := httptest.NewRecorder()
	h.serveGeminiSetupScript(replayRR, replay)
	if replayRR.Code != http.StatusNotFound {
		t.Fatalf("nonce replay status = %d, want 404", replayRR.Code)
	}
}

func TestServeAntigravitySetupScript_Bash(t *testing.T) {
	secret := "test-secret-key-12345678901234567890"
	t.Setenv("POOL_JWT_SECRET", secret)
	t.Setenv("PUBLIC_URL", "")

	h, nonce := newSetupScriptHandler(t)
	req := httptest.NewRequest(http.MethodGet, "http://example.com/setup/antigravity/"+nonce, nil)
	rr := httptest.NewRecorder()
	h.serveAntigravitySetupScript(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/x-shellscript") {
		t.Fatalf("Content-Type = %q, want text/x-shellscript*", ct)
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store (script embeds a credential)", cc)
	}
	body := rr.Body.String()
	for _, want := range []string{
		"AIzaSy-pool-",
		`export GEMINI_API_KEY="AIzaSy-pool-`,
		`export GOOGLE_GEMINI_BASE_URL="http://example.com"`,
		`"modelProvider": "gemini"`,
		".gemini/antigravity-cli",
		"# >>> Antigravity Pool Configuration >>>",
		"# <<< Antigravity Pool Configuration <<<",
		"command -v agy",
		"https://antigravity.google/cli/install.sh",
		"python3",
		"node",
		"tmp_fd, tmp = tempfile.mkstemp",
		"os.fsync(handle.fileno())",
		"os.replace(tmp, path)",
		"fs.fsyncSync(fd)",
		"fs.renameSync(tmp,p)",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected Antigravity bash setup to contain %q, got:\n%s", want, body)
		}
	}

	legacy := httptest.NewRequest(http.MethodGet, "http://example.com/setup/antigravity/old-legacy-token", nil)
	legacyRR := httptest.NewRecorder()
	h.serveAntigravitySetupScript(legacyRR, legacy)
	if legacyRR.Code != http.StatusNotFound {
		t.Fatalf("legacy token status = %d, want 404", legacyRR.Code)
	}

	// The Antigravity script embeds the credential, so its redemption is single-use.
	replay := httptest.NewRequest(http.MethodGet, "http://example.com/setup/antigravity/"+nonce, nil)
	replayRR := httptest.NewRecorder()
	h.serveAntigravitySetupScript(replayRR, replay)
	if replayRR.Code != http.StatusNotFound {
		t.Fatalf("nonce replay status = %d, want 404", replayRR.Code)
	}
}

func TestServeAntigravitySetupScript_BashIdempotent(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash script semantics are verified on Linux and CI")
	}
	secret := "test-secret-key-12345678901234567890"
	t.Setenv("POOL_JWT_SECRET", secret)
	t.Setenv("PUBLIC_URL", "")

	h, nonce := newSetupScriptHandler(t)
	req := httptest.NewRequest(http.MethodGet, "http://example.com/setup/antigravity/"+nonce, nil)
	rr := httptest.NewRecorder()
	h.serveAntigravitySetupScript(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}

	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".zshrc"), []byte("export EDITOR=vim\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(home, ".bashrc"), []byte("export PAGER=less\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	settingsDir := filepath.Join(home, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(settingsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	settingsFile := filepath.Join(settingsDir, "settings.json")
	if err := os.WriteFile(settingsFile, []byte("{\n  \"theme\": \"dark\"\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	for range 2 {
		cmd := exec.Command("bash")
		cmd.Stdin = strings.NewReader(rr.Body.String())
		cmd.Env = append(os.Environ(), "HOME="+home, "SHELL=/bin/bash")
		if output, err := cmd.CombinedOutput(); err != nil {
			t.Fatalf("run installer: %v\n%s", err, output)
		}
	}

	settings, err := os.ReadFile(settingsFile)
	if err != nil {
		t.Fatal(err)
	}
	settingsText := string(settings)
	if !strings.Contains(settingsText, `"modelProvider": "gemini"`) || !strings.Contains(settingsText, `"theme": "dark"`) {
		t.Fatalf("settings.json must keep existing keys and select gemini:\n%s", settingsText)
	}
	if count := strings.Count(settingsText, "modelProvider"); count != 1 {
		t.Fatalf("modelProvider count = %d, want 1:\n%s", count, settingsText)
	}
	backups, err := filepath.Glob(settingsFile + ".bak.*")
	if err != nil || len(backups) > 1 {
		t.Fatalf("expected at most one versioned backup, got %v (err=%v)", backups, err)
	}
	if leftovers, _ := filepath.Glob(filepath.Join(settingsDir, ".settings.*.tmp")); len(leftovers) != 0 {
		t.Fatalf("atomic write left temp files behind: %v", leftovers)
	}

	// Both existing profiles must carry the pool block exactly once.
	for _, profileName := range []string{".zshrc", ".bashrc"} {
		profileBytes, err := os.ReadFile(filepath.Join(home, profileName))
		if err != nil {
			t.Fatal(err)
		}
		profileText := string(profileBytes)
		if !strings.Contains(profileText, `export GEMINI_API_KEY="AIzaSy-pool-`) || !strings.Contains(profileText, `export GOOGLE_GEMINI_BASE_URL="http://example.com"`) {
			t.Fatalf("%s missing pool env exports:\n%s", profileName, profileText)
		}
		if count := strings.Count(profileText, "# >>> Antigravity Pool Configuration >>>"); count != 1 {
			t.Fatalf("%s pool block count = %d, want 1:\n%s", profileName, count, profileText)
		}
	}
	if !strings.Contains(string(mustReadFile(t, filepath.Join(home, ".zshrc"))), "export EDITOR=vim") {
		t.Fatalf("existing .zshrc content dropped:\n%s", mustReadFile(t, filepath.Join(home, ".zshrc")))
	}
	if !strings.Contains(string(mustReadFile(t, filepath.Join(home, ".bashrc"))), "export PAGER=less") {
		t.Fatalf("existing .bashrc content dropped:\n%s", mustReadFile(t, filepath.Join(home, ".bashrc")))
	}
}

func TestServeAntigravitySetupScript_BashInvalidSettingsAborts(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash script semantics are verified on Linux and CI")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		if _, nodeErr := exec.LookPath("node"); nodeErr != nil {
			t.Skip("no JSON validator (python3/node) available for the abort path")
		}
	}
	secret := "test-secret-key-12345678901234567890"
	t.Setenv("POOL_JWT_SECRET", secret)
	t.Setenv("PUBLIC_URL", "")

	h, nonce := newSetupScriptHandler(t)
	req := httptest.NewRequest(http.MethodGet, "http://example.com/setup/antigravity/"+nonce, nil)
	rr := httptest.NewRecorder()
	h.serveAntigravitySetupScript(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}

	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".bashrc"), []byte("export EDITOR=vim\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	settingsDir := filepath.Join(home, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(settingsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	settingsFile := filepath.Join(settingsDir, "settings.json")
	broken := "{\n  \"theme\": \"dark\"\n"
	if err := os.WriteFile(settingsFile, []byte(broken), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash")
	cmd.Stdin = strings.NewReader(rr.Body.String())
	cmd.Env = append(os.Environ(), "HOME="+home, "SHELL=/bin/bash")
	output, err := cmd.CombinedOutput()
	if err == nil {
		t.Fatalf("installer must abort on invalid settings.json:\n%s", output)
	}
	if !strings.Contains(string(output), "A copy was saved as") {
		t.Fatalf("installer must point at the settings.json backup:\n%s", output)
	}

	// The original file survives via backup, and profiles are left untouched.
	current, readErr := os.ReadFile(settingsFile)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if !strings.Contains(string(current), `"theme": "dark"`) || strings.Contains(string(current), "modelProvider") {
		t.Fatalf("broken settings.json was modified in place:\n%s", current)
	}
	backups, globErr := filepath.Glob(settingsFile + ".bak.*")
	if globErr != nil || len(backups) != 1 {
		t.Fatalf("expected one backup of the broken file, got %v (err=%v)", backups, globErr)
	}
	bashrc := string(mustReadFile(t, filepath.Join(home, ".bashrc")))
	if strings.Contains(bashrc, "Antigravity Pool Configuration") {
		t.Fatalf("profile was modified before settings.json aborted:\n%s", bashrc)
	}
}

func mustReadFile(t *testing.T, path string) []byte {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func TestServeAntigravitySetupScript_BashNullProvider(t *testing.T) {
	if runtime.GOOS == "windows" {
		t.Skip("bash script semantics are verified on Linux and CI")
	}
	if _, err := exec.LookPath("python3"); err != nil {
		if _, nodeErr := exec.LookPath("node"); nodeErr != nil {
			t.Skip("no JSON parser (python3/node) available for the merge path")
		}
	}
	secret := "test-secret-key-12345678901234567890"
	t.Setenv("POOL_JWT_SECRET", secret)
	t.Setenv("PUBLIC_URL", "")

	h, nonce := newSetupScriptHandler(t)
	req := httptest.NewRequest(http.MethodGet, "http://example.com/setup/antigravity/"+nonce, nil)
	rr := httptest.NewRecorder()
	h.serveAntigravitySetupScript(rr, req)
	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}

	home := t.TempDir()
	if err := os.WriteFile(filepath.Join(home, ".bashrc"), []byte("export EDITOR=vim\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	settingsDir := filepath.Join(home, ".gemini", "antigravity-cli")
	if err := os.MkdirAll(settingsDir, 0o700); err != nil {
		t.Fatal(err)
	}
	settingsFile := filepath.Join(settingsDir, "settings.json")
	// A text-match edit reports success on modelProvider=null without fixing
	// it; the parser-based edit must set the string value.
	if err := os.WriteFile(settingsFile, []byte("{\n  \"modelProvider\": null,\n  \"theme\": \"dark\"\n}\n"), 0o600); err != nil {
		t.Fatal(err)
	}

	cmd := exec.Command("bash")
	cmd.Stdin = strings.NewReader(rr.Body.String())
	cmd.Env = append(os.Environ(), "HOME="+home, "SHELL=/bin/bash")
	if output, err := cmd.CombinedOutput(); err != nil {
		t.Fatalf("run installer: %v\n%s", err, output)
	}

	settingsText := string(mustReadFile(t, settingsFile))
	if !strings.Contains(settingsText, `"modelProvider": "gemini"`) {
		t.Fatalf("modelProvider=null was not replaced by the parser edit:\n%s", settingsText)
	}
	if strings.Contains(settingsText, "modelProvider\": null") || !strings.Contains(settingsText, `"theme": "dark"`) {
		t.Fatalf("parser edit lost or kept the wrong value:\n%s", settingsText)
	}
}

func TestServeAntigravitySetupScript_PowerShell(t *testing.T) {
	secret := "test-secret-key-12345678901234567890"
	t.Setenv("POOL_JWT_SECRET", secret)
	t.Setenv("PUBLIC_URL", "")

	h, nonce := newSetupScriptHandler(t)
	req := httptest.NewRequest(http.MethodGet, "http://example.com/setup/antigravity/"+nonce+"?shell=powershell", nil)
	rr := httptest.NewRecorder()
	h.serveAntigravitySetupScript(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type = %q, want text/plain*", ct)
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store", cc)
	}
	body := rr.Body.String()
	for _, want := range []string{
		"$env:GEMINI_API_KEY",
		"$env:GOOGLE_GEMINI_BASE_URL",
		"AIzaSy-pool-",
		".gemini\\antigravity-cli",
		"ConvertFrom-Json",
		"Add-Member -NotePropertyName modelProvider",
		"[Environment]::SetEnvironmentVariable('GEMINI_API_KEY', $ApiKey, 'User')",
		"[Environment]::SetEnvironmentVariable('GOOGLE_GEMINI_BASE_URL', $BaseUrl, 'User')",
		"Copy-Item $settingsPath $backup",
		"Set-Utf8NoBomAtomic -Path $settingsPath -Value $json",
		"[System.IO.File]::Replace($tmp, $Path, $null)",
		"$stream.Flush($true)",
		"Get-Command agy -ErrorAction SilentlyContinue",
		"https://antigravity.google/cli/install.ps1",
		"# >>> Antigravity Pool Configuration >>>",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected PowerShell script to contain %q, got:\n%s", want, body)
		}
	}
	if strings.Contains(body, "`") {
		t.Fatalf("PowerShell script should not contain backticks (Go raw string safety), got:\n%s", body)
	}
}

func TestServeAntigravitySettingsConfig(t *testing.T) {
	secret := "test-secret-key-12345678901234567890"
	t.Setenv("POOL_JWT_SECRET", secret)
	t.Setenv("PUBLIC_URL", "")

	h, nonce := newSetupScriptHandler(t)
	req := httptest.NewRequest(http.MethodGet, "http://example.com/config/antigravity/"+nonce, nil)
	rr := httptest.NewRecorder()
	h.serveConfigDownload(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	if cc := rr.Header().Get("Cache-Control"); cc != "no-store" {
		t.Fatalf("Cache-Control = %q, want no-store (response embeds a credential)", cc)
	}
	body := rr.Body.String()
	for _, want := range []string{
		`"api_key":"AIzaSy-pool-`,
		`"base_url":"http://example.com"`,
		`"modelProvider":"gemini"`,
		`"GEMINI_API_KEY":"AIzaSy-pool-`,
		`"GOOGLE_GEMINI_BASE_URL":"http://example.com"`,
		`"settings_file":"~/.gemini/antigravity-cli/settings.json"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected Antigravity config to contain %q, got:\n%s", want, body)
		}
	}
}

func TestPassportSPAServesReactSignalRoom(t *testing.T) {
	h := &proxyHandler{cfg: &config{}}
	req := httptest.NewRequest(http.MethodGet, "http://example.com/app", nil)
	rr := httptest.NewRecorder()

	h.servePassportSPA(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	body := rr.Body.String()
	for _, want := range []string{
		`<div id="root"></div>`,
		`AI Pool`,
		`src="/assets/`,
		`href="/assets/`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected Passport SPA to contain %q", want)
		}
	}
}

func TestServeSignalRoomAsset(t *testing.T) {
	h := &proxyHandler{cfg: &config{}}
	page := httptest.NewRecorder()
	h.servePassportSPA(page, httptest.NewRequest(http.MethodGet, "http://example.com/app", nil))
	body := page.Body.String()
	start := strings.Index(body, `src="/assets/`)
	if start < 0 {
		t.Fatal("signal room script asset missing")
	}
	start += len(`src="`)
	end := strings.Index(body[start:], `"`)
	if end < 0 {
		t.Fatal("signal room script asset is malformed")
	}
	assetPath := body[start : start+end]

	rr := httptest.NewRecorder()
	h.serveSignalRoomAsset(rr, httptest.NewRequest(http.MethodGet, "http://example.com"+assetPath, nil))
	if rr.Code != http.StatusOK || rr.Body.Len() == 0 {
		t.Fatalf("asset response status=%d bytes=%d", rr.Code, rr.Body.Len())
	}
	if got := rr.Header().Get("Content-Type"); !strings.Contains(got, "javascript") {
		t.Fatalf("Content-Type = %q, want JavaScript", got)
	}
	if got := rr.Header().Get("Cache-Control"); !strings.Contains(got, "immutable") {
		t.Fatalf("Cache-Control = %q, want immutable", got)
	}
}

func TestServeHeroImageWebP(t *testing.T) {
	h := &proxyHandler{}
	req := httptest.NewRequest(http.MethodGet, "http://example.com/hero.webp", nil)
	rr := httptest.NewRecorder()

	h.serveHeroImage(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	if got := rr.Header().Get("Content-Type"); got != "image/webp" {
		t.Fatalf("Content-Type = %q, want image/webp", got)
	}
	body := rr.Body.Bytes()
	if len(body) < 12 || string(body[:4]) != "RIFF" || string(body[8:12]) != "WEBP" {
		t.Fatalf("hero response is not WebP: %q", body[:min(len(body), 12)])
	}
}

func TestServeCuteCodeSetupScript_Bash(t *testing.T) {
	secret := "test-secret-key-12345678901234567890"
	t.Setenv("POOL_JWT_SECRET", secret)
	t.Setenv("PUBLIC_URL", "")

	h, nonce := newSetupScriptHandler(t)
	req := httptest.NewRequest(http.MethodGet, "http://example.com/setup/cute-code/"+nonce, nil)
	rr := httptest.NewRecorder()
	h.serveCuteCodeSetupScript(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	body := rr.Body.String()
	for _, want := range []string{
		"https://git.irrigate.cc/pp/cute-code/raw/branch/main/install.sh",
		"/config/cute-code/" + nonce,
		"CLAUDE_DIR=\"${CLAUDE_CONFIG_DIR:-$HOME/.claude}\"",
		"cute-code --model gpt-6-astra",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected cute-code bash setup to contain %q, got:\n%s", want, body)
		}
	}

	// Setup only peeked: the config fetch is the single redemption.
	first := httptest.NewRecorder()
	h.serveCuteCodeSettingsConfig(first, httptest.NewRequest(http.MethodGet, "http://example.com/config/cute-code/"+nonce, nil))
	if first.Code != http.StatusOK {
		t.Fatalf("config fetch after setup = %d, want 200", first.Code)
	}
	second := httptest.NewRecorder()
	h.serveCuteCodeSettingsConfig(second, httptest.NewRequest(http.MethodGet, "http://example.com/config/cute-code/"+nonce, nil))
	if second.Code != http.StatusNotFound {
		t.Fatalf("config replay = %d, want 404", second.Code)
	}

	legacy := httptest.NewRecorder()
	h.serveCuteCodeSetupScript(legacy, httptest.NewRequest(http.MethodGet, "http://example.com/setup/cute-code/old-legacy-token", nil))
	if legacy.Code != http.StatusNotFound {
		t.Fatalf("legacy token status = %d, want 404", legacy.Code)
	}
}

func TestServeCuteCodeSetupScript_PowerShell(t *testing.T) {
	secret := "test-secret-key-12345678901234567890"
	t.Setenv("POOL_JWT_SECRET", secret)
	t.Setenv("PUBLIC_URL", "")

	h, nonce := newSetupScriptHandler(t)
	req := httptest.NewRequest(http.MethodGet, "http://example.com/setup/cute-code/"+nonce+"?shell=powershell", nil)
	rr := httptest.NewRecorder()
	h.serveCuteCodeSetupScript(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	body := rr.Body.String()
	for _, want := range []string{
		"https://git.irrigate.cc/pp/cute-code/raw/branch/main/install.ps1",
		"/config/cute-code/" + nonce,
		"$claudeDir = $env:CLAUDE_CONFIG_DIR",
		"cute-code --model gpt-6-astra",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected cute-code PowerShell setup to contain %q, got:\n%s", want, body)
		}
	}
}

func TestServeCuteCodeSettingsConfig(t *testing.T) {
	secret := "test-secret-key-12345678901234567890"
	t.Setenv("POOL_JWT_SECRET", secret)
	t.Setenv("PUBLIC_URL", "")

	h, nonce := newSetupScriptHandler(t)
	req := httptest.NewRequest(http.MethodGet, "http://example.com/config/cute-code/"+nonce, nil)
	rr := httptest.NewRecorder()
	h.serveCuteCodeSettingsConfig(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	body := rr.Body.String()
	for _, want := range []string{
		`"openaiBaseUrl": "http://example.com"`,
		`"anthropicBaseUrl": "http://example.com"`,
		`"openaiApiKey": "sk-ant-oat01-pool-`,
		`"model": "gpt-6-astra"`,
		`"id": "gpt-6-astra"`,
		`"id": "gpt-5.6-sol"`,
		`"id": "gpt-5.5"`,
		`"id": "claude-fable-5-1"`,
		`"id": "claude-fable-5"`,
		`"id": "claude-opus-4-8"`,
		`"id": "claude-opus-5"`,
		`"id": "MiniMax-M3"`,
		`"id": "MiniMax-M2.7"`,
		`"id": "glm-5.3"`,
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected cute-code config to contain %q, got:\n%s", want, body)
		}
	}
	for _, forbidden := range []string{"remoteCompactForAnthropic", "remoteCompactModel"} {
		if strings.Contains(body, forbidden) {
			t.Fatalf("cute-code config should not contain %q, got:\n%s", forbidden, body)
		}
	}
}

func TestServeClaudeSetupScript_BashClearsConflictingClaudeAuth(t *testing.T) {
	secret := "test-secret-key-12345678901234567890"
	t.Setenv("POOL_JWT_SECRET", secret)
	t.Setenv("PUBLIC_URL", "")

	h, nonce := newSetupScriptHandler(t)
	req := httptest.NewRequest(http.MethodGet, "http://example.com/setup/claude/"+nonce, nil)
	rr := httptest.NewRecorder()
	h.serveClaudeSetupScript(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	body := rr.Body.String()
	for _, want := range []string{
		"CONFLICTING_ENV_VARS=(",
		"unset ANTHROPIC_AUTH_TOKEN",
		"unset ANTHROPIC_API_KEY",
		"CLAUDE_DIR=\"${CLAUDE_CONFIG_DIR:-$HOME/.claude}\"",
		"delete settings.apiKeyHelper;",
		"settings.pop('apiKeyHelper', None)",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected bash script to contain %q, got:\n%s", want, body)
		}
	}

	legacy := httptest.NewRequest(http.MethodGet, "http://example.com/setup/claude/old-legacy-token", nil)
	legacyRR := httptest.NewRecorder()
	h.serveClaudeSetupScript(legacyRR, legacy)
	if legacyRR.Code != http.StatusNotFound {
		t.Fatalf("legacy token status = %d, want 404", legacyRR.Code)
	}

	// The Claude script embeds the credential, so its redemption is single-use.
	replay := httptest.NewRequest(http.MethodGet, "http://example.com/setup/claude/"+nonce, nil)
	replayRR := httptest.NewRecorder()
	h.serveClaudeSetupScript(replayRR, replay)
	if replayRR.Code != http.StatusNotFound {
		t.Fatalf("nonce replay status = %d, want 404", replayRR.Code)
	}
}

func TestServeClaudeSetupScript_PowerShell(t *testing.T) {
	secret := "test-secret-key-12345678901234567890"
	t.Setenv("POOL_JWT_SECRET", secret)

	// Ensure env is not contaminated by user-specific settings during test runs.
	t.Setenv("PUBLIC_URL", "")

	h, nonce := newSetupScriptHandler(t)

	req := httptest.NewRequest(http.MethodGet, "http://example.com/setup/claude/"+nonce+"?shell=powershell", nil)
	rr := httptest.NewRecorder()
	h.serveClaudeSetupScript(rr, req)

	if rr.Code != http.StatusOK {
		t.Fatalf("status = %d, want %d", rr.Code, http.StatusOK)
	}
	if ct := rr.Header().Get("Content-Type"); !strings.HasPrefix(ct, "text/plain") {
		t.Fatalf("Content-Type = %q, want text/plain*", ct)
	}
	body := rr.Body.String()
	if !strings.Contains(body, "$env:ANTHROPIC_BASE_URL = $BaseUrl") {
		t.Fatalf("expected PowerShell env setup in body, got:\n%s", body)
	}
	for _, want := range []string{
		"[Environment]::SetEnvironmentVariable('CLAUDE_CODE_OAUTH_TOKEN', $OAuthToken, 'User')",
		"[Environment]::SetEnvironmentVariable($name, $null, 'User')",
		"Remove-ObjectProperty -Object $settings -Name 'apiKeyHelper'",
		"foreach ($name in $conflictingEnvVars) { Remove-ObjectProperty -Object $envObj -Name $name }",
		"$claudeDir = $env:CLAUDE_CONFIG_DIR",
	} {
		if !strings.Contains(body, want) {
			t.Fatalf("expected PowerShell script to contain %q, got:\n%s", want, body)
		}
	}
	if !strings.Contains(body, "ConvertTo-Json -Depth 10") {
		t.Fatalf("expected PowerShell JSON update logic in body, got:\n%s", body)
	}
	if strings.Contains(body, "`") {
		t.Fatalf("PowerShell script should not contain backticks (Go raw string safety), got:\n%s", body)
	}
}
