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
		"pool.example:8989":  "http://pool.example:8989",
		"[::1]:8989":         "http://[::1]:8989",
		"localhost:8989":     "http://localhost:8989",
		"$(id)":              "",
		"x';calc;'":          "",
		"":                   "",
		"pool.example:0":     "",
		"pool.example:99999": "",
		"[:::]:8989":         "",
	} {
		req := httptest.NewRequest(http.MethodGet, "http://example.com/setup/codex/token", nil)
		req.Host = host
		got, err := h.getEffectivePublicURL(req)
		if got != want || (err != nil) != (want == "") {
			t.Errorf("Host %q inferred %q (%v), want %q", host, got, err, want)
		}
	}
}

func TestInvalidSetupOriginDoesNotConsumeCredentials(t *testing.T) {
	t.Setenv("PUBLIC_URL", "")
	t.Setenv("POOL_JWT_SECRET", "synthetic-invalid-origin-secret")
	passport, client := newNonceTestPassport(t)
	h := &proxyHandler{cfg: &config{}, passport: passport}
	routes := map[string]func(http.ResponseWriter, *http.Request){
		"/setup/codex/":        h.serveCodexSetupScript,
		"/setup/gemini/":       h.serveGeminiSetupScript,
		"/setup/antigravity/":  h.serveAntigravitySetupScript,
		"/setup/claude/":       h.serveClaudeSetupScript,
		"/setup/cute-code/":    h.serveCuteCodeSetupScript,
		"/setup/grok/":         h.serveGrokSetupScript,
		"/setup/pi/":           h.servePiSetupScript,
		"/config/codex/":       h.serveConfigDownload,
		"/config/gemini/":      h.serveConfigDownload,
		"/config/antigravity/": h.serveConfigDownload,
		"/config/claude/":      h.serveConfigDownload,
		"/config/pi/":          h.serveConfigDownload,
		"/config/grok/":        h.serveConfigDownload,
		"/config/cute-code/":   h.serveCuteCodeSettingsConfig,
	}
	for prefix, serve := range routes {
		for _, host := range []string{"", "$(id)", "pool.example:99999"} {
			for _, query := range []string{"", "shell=powershell"} {
				nonce, _, err := passport.mintConfigDownloadNonce(client)
				if err != nil {
					t.Fatal(err)
				}
				req := httptest.NewRequest(http.MethodGet, "http://pool.example"+prefix+nonce, nil)
				req.Host, req.URL.RawQuery = host, query
				w := httptest.NewRecorder()
				serve(w, req)
				if w.Code != http.StatusBadRequest {
					t.Fatalf("%s host %q: status=%d body=%s", prefix, host, w.Code, w.Body.String())
				}
				if strings.Contains(w.Body.String(), "localhost") || strings.Contains(w.Body.String(), nonce) {
					t.Fatal("invalid origin produced configuration content")
				}
				if passport.peekConfigDownloadNonce(nonce) == nil {
					t.Fatalf("%s consumed the nonce on an invalid origin", prefix)
				}
			}
		}
	}
	nonce, _, err := passport.mintConfigDownloadNonce(client)
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "http://pool.example/", nil)
	req.Host = "$(id)"
	if _, _, err := h.mintSetupURLs(req, client); err == nil {
		t.Fatal("minted setup links for an invalid origin")
	}
	if passport.peekConfigDownloadNonce(nonce) == nil {
		t.Fatal("invalid origin revoked the previous valid setup link")
	}
	if _, err := h.webAuthnForRequest(req); err == nil {
		t.Fatal("passkeys accepted an invalid origin")
	}
	if _, err := antigravityOAuthTargetOrigin(req, h); err == nil {
		t.Fatal("OAuth accepted an invalid origin")
	}
}

func TestPublicOriginUsesConfiguredURLOrValidRequestTransport(t *testing.T) {
	t.Setenv("PUBLIC_URL", "")
	h := &proxyHandler{}
	req := httptest.NewRequest(http.MethodGet, "https://pool.example/", nil)
	if got, err := h.getEffectivePublicURL(req); err != nil || got != "https://pool.example" {
		t.Fatalf("TLS origin=%q, %v", got, err)
	}
	req.TLS = nil
	req.Header.Set("X-Forwarded-Proto", "https")
	if got, err := h.getEffectivePublicURL(req); err != nil || got != "https://pool.example" {
		t.Fatalf("forwarded origin=%q, %v", got, err)
	}
	req.Header.Set("Origin", "not-an-origin")
	if _, err := antigravityOAuthTargetOrigin(req, h); err == nil {
		t.Fatal("invalid explicit Origin was replaced with the request origin")
	}
	t.Setenv("PUBLIC_URL", "https://configured.example")
	req.Host = "$(id)"
	if got, err := h.getEffectivePublicURL(req); err != nil || got != "https://configured.example" {
		t.Fatalf("configured origin=%q, %v", got, err)
	}
}
