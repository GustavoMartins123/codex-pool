package main

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestHTTPRouteContract(t *testing.T) {
	const adminToken = "route-contract-admin"
	const requestID = "route-contract-request"
	h := &proxyHandler{cfg: &config{adminToken: adminToken}}

	cases := []struct {
		name, method, target string
		status               int
		body                 string
		admin                bool
		allow                string
	}{
		{name: "passport config", method: "GET", target: "/api/auth/config", status: 200, body: `"operator_exists":false`},
		{name: "passport config rejects HEAD", method: "HEAD", target: "/api/auth/config", status: 405, body: "method not allowed"},
		{name: "passport slash is distinct", method: "GET", target: "/api/auth/config/", status: 401, body: "valid pool token required"},
		{name: "passport matches decoded path", method: "GET", target: "/api%2fauth/config", status: 200, body: `"operator_exists":false`},
		{name: "passport repeated slash", method: "GET", target: "/api//auth/config", status: 401, body: "valid pool token required"},
		{name: "passport dot segment", method: "GET", target: "/api/auth/./config", status: 401, body: "valid pool token required"},
		{name: "passport encoded dot segment", method: "GET", target: "/api/auth/%2e/config", status: 401, body: "valid pool token required"},
		{name: "passport identity", method: "GET", target: "/api/auth/me", status: 401, body: `"unauthorized"`},
		{name: "session subresource", method: "POST", target: "/api/sessions/test/events", status: 401, body: "authentication required"},
		{name: "conversation subresource", method: "GET", target: "/api/conversations/test", status: 401, body: "authentication required"},
		{name: "admin auth before method", method: "POST", target: "/admin/accounts", status: 401, body: "invalid admin token"},
		{name: "admin rejects unsupported method", method: "POST", target: "/admin/accounts", status: 405, body: "method not allowed", admin: true},
		{name: "admin protects decoded subresource", method: "GET", target: "/admin/accounts/team%2faccount/disable", status: 401, body: "invalid admin token"},
		{name: "admin matches account suffix", method: "GET", target: "/admin/accounts/team//account/disable", status: 405, body: "method not allowed", admin: true},
		{name: "admin unknown account subresource", method: "GET", target: "/admin/accounts/test/unknown", status: 401, body: "valid pool token required"},
		{name: "admin provider prefix", method: "GET", target: "/admin/claude-unknown", status: 401, body: "invalid admin token"},
		{name: "public Claude callback method", method: "HEAD", target: "/admin/claude/callback", status: 404, body: "404 page not found"},
		{name: "Claude callback slash requires auth", method: "GET", target: "/admin/claude/callback/", status: 401, body: "invalid admin token"},
		{name: "public Antigravity callback method", method: "POST", target: "/admin/antigravity/callback", status: 405, body: "method not allowed"},
		{name: "profile prefix without slash", method: "GET", target: "/debug/pprof", status: 401, body: "valid pool token required"},
		{name: "profile prefix requires auth", method: "GET", target: "/debug/pprof/", status: 401, body: "invalid admin token"},
		{name: "contribution auth before lookup", method: "POST", target: "/api/pool/accounts/unknown", status: 401, body: "invalid admin token"},
		{name: "contribution method before lookup", method: "GET", target: "/api/pool/accounts/unknown", status: 405, body: "method not allowed", admin: true},
		{name: "unknown contribution", method: "POST", target: "/api/pool/accounts/unknown", status: 404, body: "404 page not found", admin: true},
		{name: "health accepts trailing slash", method: "HEAD", target: "/livez/", status: 200, body: `"status":"ok"`},
		{name: "health accepts POST", method: "POST", target: "/livez", status: 200, body: `"status":"ok"`},
		{name: "CLI SSE negotiation", method: "OPTIONS", target: "/client/ps/mcp/", status: 204, allow: "GET, HEAD, POST, OPTIONS"},
		{name: "CLI reset credits", method: "POST", target: "/client/rate-limit-reset-credits/consume", status: 200, body: `"code":"no_credit"`},
		{name: "proxy requires credentials", method: "POST", target: "/v1/responses", status: 401, body: "valid pool token required"},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			r := httptest.NewRequest(tc.method, tc.target, nil)
			r.Header.Set("X-Request-ID", " "+requestID+" ")
			if tc.admin {
				r.Header.Set("X-Admin-Token", adminToken)
			}
			w := httptest.NewRecorder()
			h.ServeHTTP(w, r)

			if w.Code != tc.status || !strings.Contains(w.Body.String(), tc.body) {
				t.Fatalf("%s %s: response = %d %q, want %d containing %q", tc.method, tc.target, w.Code, w.Body.String(), tc.status, tc.body)
			}
			if got := w.Header().Get("Allow"); got != tc.allow {
				t.Errorf("Allow = %q, want %q", got, tc.allow)
			}
			if location := w.Header().Get("Location"); location != "" {
				t.Errorf("unexpected redirect to %q", location)
			}
			if got := w.Header().Get("X-Pool-Request-Id"); got != requestID {
				t.Errorf("X-Pool-Request-Id = %q, want %q", got, requestID)
			}
			if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
				t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
			}
		})
	}
}

func TestHTTPRouteRejectsInvalidPeerBeforeRequestID(t *testing.T) {
	h := &proxyHandler{}
	r := httptest.NewRequest(http.MethodGet, "/livez", nil)
	r.RemoteAddr = "invalid-peer"
	r.Header.Set("X-Request-ID", "should-not-be-used")
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusBadRequest || w.Body.String() != "invalid remote peer IP\n" {
		t.Fatalf("response = %d %q, want invalid remote peer IP", w.Code, w.Body.String())
	}
	if got := w.Header().Get("X-Pool-Request-Id"); got != "" {
		t.Errorf("unexpected X-Pool-Request-Id %q", got)
	}
	if got := w.Header().Get("X-Content-Type-Options"); got != "nosniff" {
		t.Errorf("X-Content-Type-Options = %q, want nosniff", got)
	}
}
