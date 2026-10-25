package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

// hostileSetupTokens break out of the quoted TOKEN/$Token assignments in the
// rendered bash and PowerShell scripts if they are ever interpolated.
var hostileSetupTokens = []string{
	"$(id)",
	"x';Start-Process calc;'",
	`x";id;"`,
	"x`id`",
	"x\nid",
	"x id",
	strings.Repeat("a", maxSetupTokenLength+1),
}

func TestValidSetupTokenAcceptsMintedNonces(t *testing.T) {
	passport, client := newNonceTestPassport(t)
	for i := 0; i < 32; i++ {
		nonce, _, err := passport.mintConfigDownloadNonce(client)
		if err != nil {
			t.Fatal(err)
		}
		if !validSetupToken(nonce) {
			t.Fatalf("minted nonce %q rejected", nonce)
		}
	}
	for _, token := range append([]string{""}, hostileSetupTokens...) {
		if validSetupToken(token) {
			t.Fatalf("hostile token %q accepted", token)
		}
	}
}

func TestSetupScriptsRejectHostileTokens(t *testing.T) {
	h := &proxyHandler{}
	routes := map[string]func(http.ResponseWriter, *http.Request){
		"/setup/codex/":       h.serveCodexSetupScript,
		"/setup/gemini/":      h.serveGeminiSetupScript,
		"/setup/antigravity/": h.serveAntigravitySetupScript,
		"/setup/claude/":      h.serveClaudeSetupScript,
		"/setup/cute-code/":   h.serveCuteCodeSetupScript,
		"/setup/grok/":        h.serveGrokSetupScript,
		"/setup/pi/":          h.servePiSetupScript,
		"/config/cute-code/":  h.serveCuteCodeSettingsConfig,
	}
	for prefix, serve := range routes {
		for _, token := range hostileSetupTokens {
			for _, query := range []string{"", "shell=powershell"} {
				req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
				req.URL.Path = prefix + token
				req.URL.RawQuery = query
				rr := httptest.NewRecorder()
				serve(rr, req)
				if rr.Code != http.StatusBadRequest {
					t.Errorf("%s%q (%s) status = %d, want 400", prefix, token, query, rr.Code)
				}
				if strings.Contains(rr.Body.String(), token) {
					t.Errorf("%s%q (%s) echoed the token", prefix, token, query)
				}
			}
		}
	}
}

func TestConfigDownloadRejectsHostileTokens(t *testing.T) {
	t.Setenv("POOL_JWT_SECRET", "test-jwt-secret-hostile-token")
	passport, _ := newNonceTestPassport(t)
	handler := &proxyHandler{cfg: &config{}, passport: passport}
	for _, token := range hostileSetupTokens {
		req := httptest.NewRequest(http.MethodGet, "http://example.com/", nil)
		req.URL.Path = "/config/codex/" + token
		rr := httptest.NewRecorder()
		handler.serveConfigDownload(rr, req)
		if rr.Code != http.StatusBadRequest {
			t.Errorf("/config/codex/%q status = %d, want 400", token, rr.Code)
		}
	}
}

func TestInferredPublicURLRejectsHostileHosts(t *testing.T) {
	t.Setenv("PUBLIC_URL", "")
	h := &proxyHandler{}
	for host, want := range map[string]string{
		"pool.example:8989": "http://pool.example:8989",
		"[::1]:8989":        "http://[::1]:8989",
		"$(id)":             "http://localhost:8989",
		"x';calc;'":         "http://localhost:8989",
		"":                  "http://localhost:8989",
	} {
		req := httptest.NewRequest(http.MethodGet, "http://example.com/setup/codex/token", nil)
		req.Host = host
		if got := h.getEffectivePublicURL(req); got != want {
			t.Errorf("Host %q inferred %q, want %q", host, got, want)
		}
	}
}
