package main

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
)

func TestBaselineSecurityHeadersOnAllSurfaces(t *testing.T) {
	base, _ := url.Parse("http://upstream.example")
	fx := newCodexProxyFixture(t, base, nil)

	// Authenticated, credential-bearing, and probe surfaces. Status codes
	// vary (401 without credentials, 200/404 depending on handler state);
	// the baseline headers must be present regardless.
	surfaces := []struct {
		path    string
		noStore bool
	}{
		{"/api/pool/stats", true},
		{"/admin/accounts", true},
		{"/metrics", true},
		{"/setup/codex/sometoken", true},
		{"/config/codex/sometoken", true},
		{"/oauth/token", true},
		{"/api/auth/config", true},
		{"/healthz", false},
		{"/livez", false},
		{"/readyz", false},
		{"/", false},
	}

	for _, s := range surfaces {
		t.Run(s.path, func(t *testing.T) {
			req, _ := http.NewRequest(http.MethodGet, fx.server.URL+s.path, nil)
			resp, err := http.DefaultClient.Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			h := resp.Header
			if got := h.Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
			}
			if got := h.Get("Referrer-Policy"); got != "no-referrer" {
				t.Errorf("Referrer-Policy = %q, want no-referrer", got)
			}
			if got := h.Get("Permissions-Policy"); got == "" {
				t.Error("Permissions-Policy missing")
			}
			if got := h.Get("Content-Security-Policy"); got == "" {
				t.Error("Content-Security-Policy missing")
			}
			if s.noStore && h.Get("Cache-Control") != "no-store" {
				t.Errorf("Cache-Control = %q, want no-store", h.Get("Cache-Control"))
			}
		})
	}
}

func TestFingerprintedAssetsKeepCacheHeaders(t *testing.T) {
	if requiresNoStore("/assets/index-abc123.js") {
		t.Fatal("fingerprinted assets must stay cacheable")
	}
	if requiresNoStore("/") {
		t.Fatal("public landing pages must stay cacheable")
	}
	for _, path := range []string{"/api/x", "/admin/x", "/setup/codex/t", "/config/codex/t", "/debug/pprof/", "/metrics", "/oauth/token"} {
		if !requiresNoStore(path) {
			t.Errorf("%s should require no-store", path)
		}
	}
}

func TestIPAccessPolicyPermitted(t *testing.T) {
	p := &ipAccessPolicy{}

	p.configure(nil, nil)
	for _, ip := range []string{"203.0.113.5", "10.0.0.1", "unknown-peer"} {
		if !p.permitted(ip) {
			t.Fatalf("unrestricted policy must permit %s", ip)
		}
	}

	p.configure([]string{"10.0.0.0/8", "192.168.1.5"}, nil)
	for _, ip := range []string{"10.1.2.3", "192.168.1.5", "127.0.0.1", "::1"} {
		if !p.permitted(ip) {
			t.Fatalf("allow list must permit %s", ip)
		}
	}
	for _, ip := range []string{"192.168.1.6", "203.0.113.5", "unknown-peer"} {
		if p.permitted(ip) {
			t.Fatalf("allow list must refuse %s", ip)
		}
	}

	p.configure(nil, []string{"203.0.113.0/24"})
	if p.permitted("203.0.113.5") {
		t.Fatal("deny list must refuse 203.0.113.5")
	}
	if !p.permitted("198.51.100.1") {
		t.Fatal("deny list must not affect 198.51.100.1")
	}

	// Deny wins over allow.
	p.configure([]string{"10.0.0.0/8"}, []string{"10.9.9.9"})
	if p.permitted("10.9.9.9") {
		t.Fatal("deny must win over allow")
	}
	if !p.permitted("10.1.1.1") {
		t.Fatal("allow must cover non-denied addresses")
	}
}

func TestServeHTTPRejectsDeniedIP(t *testing.T) {
	base, _ := url.Parse("http://upstream.example")
	fx := newCodexProxyFixture(t, base, nil)

	if err := globalIPAccess.configure(nil, []string{"203.0.113.0/24"}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = globalIPAccess.configure(nil, nil) })

	req, _ := http.NewRequest(http.MethodGet, fx.server.URL+"/healthz", nil)
	req.RemoteAddr = "203.0.113.7:41234"
	w := httptest.NewRecorder()
	fx.handler.ServeHTTP(w, req)
	if w.Code != http.StatusForbidden {
		t.Fatalf("status = %d, want 403", w.Code)
	}

	// A spoofed X-Forwarded-For must not bypass the policy: the peer is not
	// a trusted proxy, so getClientIP keeps the real address.
	req2, _ := http.NewRequest(http.MethodGet, fx.server.URL+"/healthz", nil)
	req2.RemoteAddr = "203.0.113.7:41234"
	req2.Header.Set("X-Forwarded-For", "127.0.0.1")
	w2 := httptest.NewRecorder()
	fx.handler.ServeHTTP(w2, req2)
	if w2.Code != http.StatusForbidden {
		t.Fatalf("status with spoofed XFF = %d, want 403", w2.Code)
	}

	// Loopback stays reachable (container health checks).
	req3, _ := http.NewRequest(http.MethodGet, fx.server.URL+"/healthz", nil)
	req3.RemoteAddr = "127.0.0.1:41234"
	w3 := httptest.NewRecorder()
	fx.handler.ServeHTTP(w3, req3)
	if w3.Code == http.StatusForbidden {
		t.Fatalf("loopback must stay permitted, got 403")
	}

	// Even rejections carry the baseline security headers.
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Fatalf("X-Content-Type-Options on 403 = %q, want nosniff", got)
	}
}
