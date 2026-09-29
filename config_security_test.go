package main

import (
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
)

func TestConfigExplicitMissingFileFailsStartup(t *testing.T) {
	if os.Getenv("POOL_TEST_MISSING_CONFIG") == "1" {
		buildConfig()
		return
	}
	path := filepath.Join(t.TempDir(), "missing.toml")
	cmd := exec.Command(os.Args[0], "-test.run=^TestConfigExplicitMissingFileFailsStartup$")
	cmd.Env = append(cmd.Environ(), "POOL_TEST_MISSING_CONFIG=1", "CONFIG_PATH="+path)
	out, err := cmd.CombinedOutput()
	if err == nil || !strings.Contains(string(out), "invalid config file") || !strings.Contains(string(out), path) {
		t.Fatalf("missing explicit config did not fail: %v %s", err, out)
	}
}
func TestConfigImplicitAbsenceAndStablePath(t *testing.T) {
	t.Chdir(t.TempDir())
	t.Setenv("CONFIG_PATH", "")
	path, cfg, err := loadStartupConfiguration()
	if err != nil || cfg != nil || !filepath.IsAbs(path) {
		t.Fatalf("env-only configuration: %s %v %v", path, cfg, err)
	}
	h := &proxyHandler{cfg: &config{configPath: path}}
	t.Setenv("CONFIG_PATH", filepath.Join(t.TempDir(), "other.toml"))
	if h.cfg.configPath != path {
		t.Fatal("resolved path changed with environment")
	}
}
func TestConfigRejectsUnknownAndInvalidSecuritySettings(t *testing.T) {
	for _, content := range []string{
		"ip_acces_allow = ['192.0.2.0/24']", "trusted_proxies = ['bad']", "ip_access_allow = ['']",
		"tier_threshold = nan", "tier_threshold = 2", "max_attempts = -1", "[routing]\ndefault_profile = 'typo'",
		"[client_policies.test.limits]\ndaily_requests = -1", "[experiments.traffic]\nenabled = true",
		"[experiments.canary.test]\ncandidate = 'model'\npercent = 101", "[max_reasoning_effort_by_user]\nu = 'typo'",
	} {
		t.Run(content, func(t *testing.T) {
			p := filepath.Join(t.TempDir(), "config.toml")
			if err := os.WriteFile(p, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			if _, err := loadConfigFile(p); err == nil {
				t.Fatalf("invalid config accepted: %s", content)
			}
		})
	}
	if _, err := loadConfigFile("config.toml.example"); err != nil {
		t.Fatalf("documented configuration rejected: %v", err)
	}
}
func TestConfigEnvironmentRejectsMalformedExplicitValues(t *testing.T) {
	for _, tc := range []struct{ key, value string }{
		{"PROXY_MAX_ATTEMPTS", "3junk"}, {"PROXY_REQUEST_TIMEOUT_SECONDS", "-1"}, {"PROXY_REQUEST_TIMEOUT_SECONDS", "9223372036854775807"},
		{"PROXY_DEBUG", "tru"}, {"PROXY_IP_PRIVACY", ""}, {"TIER_THRESHOLD", "NaN"}, {"TIER_THRESHOLD", "Inf"}, {"PROXY_IP_ALLOW", "192.0.2.1,,"}, {"PROXY_TRUSTED_PROXIES", "bad"},
	} {
		t.Run(tc.key+tc.value, func(t *testing.T) {
			t.Setenv(tc.key, tc.value)
			if err := validateConfigEnvironment(); err == nil || !strings.Contains(err.Error(), tc.key) && tc.key != "PROXY_TRUSTED_PROXIES" {
				t.Fatalf("bad explicit environment accepted: %v", err)
			}
		})
	}
	if _, err := parseInt64("12junk"); err == nil {
		t.Fatal("integer parser accepted trailing junk")
	}
}
func resetNetworkAfterTest(t *testing.T) {
	t.Helper()
	t.Cleanup(func() { globalIPAccess.configure(nil, nil); setTrustedProxies(nil) })
}
func TestConfigReloadRejectsWholeCandidate(t *testing.T) {
	resetNetworkAfterTest(t)
	for _, bad := range []string{"[routing]\ndefault_profile='typo'", "ip_access_allow=['bad']", "trusted_proxies=['bad']", "[experiments.traffic]\nenabled=true", "[client_policies.test.limits]\ndaily_requests=-1"} {
		t.Run(bad, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.toml")
			h := &proxyHandler{cfg: &config{}, pool: newPoolState(nil, false), aliases: newModelAliases(map[string]string{"probe": "previous"})}
			h.cfg.setHotReloadable(.7, RoutingConfigFile{}, map[string]ClientPolicy{"old": {}}, ExperimentsConfig{})
			globalIPAccess.configure([]string{"192.0.2.0/24"}, nil)
			setTrustedProxies([]string{"198.51.100.1"})
			content := "debug=true\ntier_threshold=.2\n" + bad
			// TOML floating point literals require a leading zero.
			content = strings.ReplaceAll(content, "=.2", "=0.2")
			if err := os.WriteFile(path, []byte(content), 0600); err != nil {
				t.Fatal(err)
			}
			pw := &poolWatcher{handler: h, configPath: path}
			if err := pw.reloadConfigCandidate(); err == nil {
				t.Fatal("invalid reload accepted")
			}
			if h.cfg.debug.Load() || h.cfg.hotTierThreshold() != .7 {
				t.Fatal("rejected reload changed settings")
			}
			if _, ok := h.cfg.hotClientPolicies()["old"]; !ok {
				t.Fatal("rejected reload changed policy")
			}
			if globalIPAccess.permitted("203.0.113.1") || !isTrustedProxy(net.ParseIP("198.51.100.1")) {
				t.Fatal("rejected reload changed network policy")
			}
		})
	}
}
func TestConfigReloadAppliesAndClearsNetworkPolicies(t *testing.T) {
	resetNetworkAfterTest(t)
	path := filepath.Join(t.TempDir(), "config.toml")
	h := &proxyHandler{cfg: &config{}, pool: newPoolState(nil, false)}
	pw := &poolWatcher{handler: h, configPath: path}
	if err := os.WriteFile(path, []byte("debug=true\nip_access_allow=['192.0.2.0/24']\ntrusted_proxies=['198.51.100.1']"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := pw.reloadConfigCandidate(); err != nil {
		t.Fatal(err)
	}
	if !h.cfg.debug.Load() || globalIPAccess.permitted("203.0.113.1") || !isTrustedProxy(net.ParseIP("198.51.100.1")) {
		t.Fatal("valid reload not applied")
	}
	if err := os.WriteFile(path, []byte("debug=false"), 0600); err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for i := 0; i < 10; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			if err := pw.reloadConfigCandidate(); err != nil {
				t.Error(err)
			}
		}()
	}
	wg.Wait()
	if h.cfg.debug.Load() || globalIPAccess.restricted() || isTrustedProxy(net.ParseIP("198.51.100.1")) {
		t.Fatal("empty network lists retained prior policy")
	}
	if err := os.Remove(path); err != nil {
		t.Fatal(err)
	}
	if err := pw.reloadConfigCandidate(); err == nil {
		t.Fatal("deleted reload file accepted")
	}
}
func TestIPAccessLoopbackOnlyBypassesReadOnlyHealth(t *testing.T) {
	resetNetworkAfterTest(t)
	setTrustedProxies(nil)
	if err := globalIPAccess.configure(nil, []string{"127.0.0.0/8", "::1", "203.0.113.1"}); err != nil {
		t.Fatal(err)
	}
	h := &proxyHandler{pool: newPoolState(nil, false)}
	for _, peer := range []string{"127.0.0.1:1234", "[::1]:1234"} {
		for _, tc := range []struct {
			method, path, xff string
			allowed           bool
		}{
			{"GET", "/healthz", "", true}, {"HEAD", "/livez", "", true}, {"GET", "/readyz", "", true},
			{"POST", "/healthz", "", false}, {"GET", "/admin/accounts", "", false}, {"POST", "/v1/responses", "", false}, {"GET", "/metrics", "", false},
			{"GET", "/livez", "127.0.0.1, 203.0.113.1", false},
		} {
			r := httptest.NewRequest(tc.method, tc.path, nil)
			r.RemoteAddr = peer
			if tc.xff != "" {
				r.Header.Set("X-Forwarded-For", tc.xff)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)
			if (w.Code != http.StatusForbidden) != tc.allowed {
				t.Fatalf("%s %s peer=%s xff=%s: %d", tc.method, tc.path, peer, tc.xff, w.Code)
			}
		}
	}
}

func TestIPAccessForwardingChainUsesNearestUntrustedHop(t *testing.T) {
	resetNetworkAfterTest(t)
	setTrustedProxies([]string{"10.0.0.0/8"})
	r := httptest.NewRequest("GET", "/livez", nil)
	r.RemoteAddr = "10.0.0.1:1234"
	r.Header.Set("X-Forwarded-For", "127.0.0.1, 203.0.113.1, 10.0.0.2")
	if ip, err := requestClientIP(r); err != nil || ip != "203.0.113.1" {
		t.Fatalf("spoofed leftmost origin accepted: %s %v", ip, err)
	}
	r.Header.Set("CF-Connecting-IP", "invalid")
	r.Header.Set("X-Real-IP", "127.0.0.1")
	if ip, err := requestClientIP(r); err == nil || ip != "" {
		t.Fatalf("invalid canonical header selected secondary origin: %s %v", ip, err)
	}
	h := &proxyHandler{}
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest {
		t.Fatalf("invalid forwarding attribution: %d", w.Code)
	}
	r.RemoteAddr = "198.51.100.1:1234"
	if ip, err := requestClientIP(r); err != nil || ip != "198.51.100.1" {
		t.Fatalf("untrusted peer used forwarding header: %s %v", ip, err)
	}
}
