package main

import (
	"net/http"
	"strings"
)

// applySecurityHeaders sets the baseline security headers on every response
// served by the pool. Surface-specific handlers may override the CSP with a
// stricter policy (Header.Set replaces the baseline wholesale); fingerprinted
// assets keep their immutable cache headers because requiresNoStore excludes
// them.
func applySecurityHeaders(w http.ResponseWriter, r *http.Request) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
	if h.Get("Content-Security-Policy") == "" {
		h.Set("Content-Security-Policy", baselineCSP)
	}
	if requestUsesHTTPS(r) {
		h.Set("Strict-Transport-Security", hstsPolicy)
	}
	if requiresNoStore(r.URL.Path) {
		h.Set("Cache-Control", "no-store")
	}
}

// baselineCSP covers every response that does not declare its own policy:
// nothing loads, nothing may frame it, and no base or form target applies.
// HTML surfaces replace it with the narrowest policy that renders them.
const baselineCSP = "default-src 'none'; frame-ancestors 'none'; base-uri 'none'; form-action 'none'"

const hstsPolicy = "max-age=31536000; includeSubDomains"

// requestUsesHTTPS reports whether the client reached the pool over HTTPS,
// directly or through a TLS-terminating proxy. Browsers ignore HSTS sent
// over plain HTTP, so a spoofed signal cannot pin an HTTP-only pool.
func requestUsesHTTPS(r *http.Request) bool {
	return r.TLS != nil || strings.EqualFold(r.Header.Get("X-Forwarded-Proto"), "https") || strings.HasPrefix(getPublicURL(), "https://")
}

// requiresNoStore reports whether the path serves authenticated or
// credential-bearing content that must never be cached by intermediaries.
// Proxied upstream traffic (the default route) is excluded: cache semantics
// there belong to the provider response, not the pool.
func requiresNoStore(path string) bool {
	return strings.HasPrefix(path, "/api/") ||
		strings.HasPrefix(path, "/admin/") ||
		strings.HasPrefix(path, "/setup/") ||
		strings.HasPrefix(path, "/config/") ||
		strings.HasPrefix(path, "/debug/") ||
		path == "/metrics" ||
		strings.HasPrefix(path, "/oauth/token")
}
