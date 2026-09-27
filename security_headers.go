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
func applySecurityHeaders(w http.ResponseWriter, path string) {
	h := w.Header()
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("Referrer-Policy", "no-referrer")
	h.Set("Permissions-Policy", "camera=(), microphone=(), geolocation=(), payment=()")
	if h.Get("Content-Security-Policy") == "" {
		h.Set("Content-Security-Policy", "frame-ancestors 'none'")
	}
	if requiresNoStore(path) {
		h.Set("Cache-Control", "no-store")
	}
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
