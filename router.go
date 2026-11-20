package main

import (
	"context"
	"encoding/json"
	"io"
	"log"
	"net"
	"net/http"
	"strings"
)

func normalizeNoopPath(path string) string {
	path = strings.TrimSpace(path)
	if path == "" {
		return "/"
	}
	// Reverse proxies and clients sometimes leave a trailing slash.
	if path != "/" {
		path = strings.TrimRight(path, "/")
	}
	return path
}

// isCodexAppsMCPPath matches the streamable-HTTP endpoint Codex uses for the
// built-in codex_apps MCP server. Path can arrive with or without the
// /backend-api prefix depending on how the public reverse proxy rewrites.
func isCodexAppsMCPPath(path string) bool {
	path = normalizeNoopPath(path)
	switch path {
	case "/api/codex/apps",
		"/backend-api/wham/apps",
		"/wham/apps",
		"/apps",
		"/backend-api/ps/mcp",
		"/api/codex/ps/mcp",
		"/ps/mcp":
		return true
	}
	return strings.HasSuffix(path, "/wham/apps") ||
		strings.HasSuffix(path, "/codex/apps") ||
		strings.HasSuffix(path, "/ps/mcp")
}

func isCodexResetCreditsPath(path string) bool {
	path = normalizeNoopPath(path)
	return strings.HasSuffix(path, "/rate-limit-reset-credits") ||
		strings.HasSuffix(path, "/rate-limit-reset-credits/consume")
}

func servePoolCodexResetCredits(w http.ResponseWriter, r *http.Request) {
	path := normalizeNoopPath(r.URL.Path)
	if strings.HasSuffix(path, "/consume") {
		respondJSON(w, map[string]any{
			"code":          "no_credit",
			"windows_reset": 0,
		})
		return
	}
	respondJSON(w, map[string]any{
		"available_count": 0,
		"credits":         []any{},
	})
}

func shouldNoopCodexPath(path string) bool {
	path = normalizeNoopPath(path)
	if isCodexAppsMCPPath(path) {
		return true
	}
	// OAuth discovery for streamable HTTP is rooted at the MCP URL path.
	if strings.HasPrefix(path, "/.well-known/oauth-authorization-server") {
		return true
	}
	switch path {
	case "/connectors/directory/list",
		"/connectors/directory/list_workspace",
		"/codex/analytics-events/events",
		"/v1/traces/ingest",
		"/plugins/featured",
		"/plugins/list",
		"/backend-api/plugins/featured",
		"/backend-api/codex/analytics-events/events":
		return true
	default:
		return false
	}
}

func serveNoopCodexPath(w http.ResponseWriter, r *http.Request) {
	path := normalizeNoopPath(r.URL.Path)
	if isCodexAppsMCPPath(path) {
		serveNoopCodexAppsMCP(w, r)
		return
	}
	if strings.HasPrefix(path, "/.well-known/oauth-authorization-server") {
		// Empty discovery doc: codex_apps does not need OAuth through the pool.
		respondJSON(w, map[string]any{
			"authorization_endpoint": "",
			"token_endpoint":         "",
			"scopes_supported":       []string{},
		})
		return
	}
	switch path {
	case "/connectors/directory/list", "/connectors/directory/list_workspace":
		respondJSON(w, map[string]any{
			"apps":      []any{},
			"nextToken": nil,
		})
	case "/v1/traces/ingest":
		respondJSON(w, map[string]any{"ok": true})
	default:
		// Empty success for noisy telemetry / plugin list probes.
		w.WriteHeader(http.StatusOK)
	}
}

func serveNoopCodexAppsMCP(w http.ResponseWriter, r *http.Request) {
	// Streamable HTTP may open with GET (SSE) or OPTIONS; neither needs tools.
	if r.Method == http.MethodGet || r.Method == http.MethodHead {
		w.Header().Set("Content-Type", "text/event-stream")
		w.Header().Set("Cache-Control", "no-cache")
		w.WriteHeader(http.StatusOK)
		return
	}
	if r.Method == http.MethodOptions {
		w.Header().Set("Allow", "GET, HEAD, POST, OPTIONS")
		w.WriteHeader(http.StatusNoContent)
		return
	}

	var req struct {
		JSONRPC string          `json:"jsonrpc"`
		ID      json.RawMessage `json:"id"`
		Method  string          `json:"method"`
		Params  json.RawMessage `json:"params"`
	}
	body, _ := io.ReadAll(io.LimitReader(r.Body, 1<<20))
	_ = json.Unmarshal(body, &req)

	// Notifications (no id) — accept and stop. Includes notifications/initialized.
	if len(req.ID) == 0 || string(req.ID) == "null" {
		w.Header().Set("Content-Type", "application/json")
		w.WriteHeader(http.StatusAccepted)
		return
	}

	result := map[string]any{}
	switch req.Method {
	case "initialize":
		// Echo the client's protocolVersion when present; clients reject
		// unsupported versions. Empty tools is intentional — pool has no apps.
		protocolVersion := "2025-06-18"
		if len(req.Params) > 0 {
			var params struct {
				ProtocolVersion string `json:"protocolVersion"`
			}
			if err := json.Unmarshal(req.Params, &params); err == nil && strings.TrimSpace(params.ProtocolVersion) != "" {
				protocolVersion = strings.TrimSpace(params.ProtocolVersion)
			}
		}
		result = map[string]any{
			"protocolVersion": protocolVersion,
			"capabilities": map[string]any{
				"tools": map[string]any{"listChanged": false},
			},
			"serverInfo": map[string]any{"name": "codex_apps", "version": "0.0.0"},
		}
	case "tools/list":
		// Empty tool list: quiet success instead of 401/handshake failure.
		result = map[string]any{"tools": []any{}}
	case "resources/list":
		result = map[string]any{"resources": []any{}}
	case "resources/templates/list":
		result = map[string]any{"resourceTemplates": []any{}}
	case "prompts/list":
		result = map[string]any{"prompts": []any{}}
	case "ping":
		result = map[string]any{}
	default:
		// Unknown methods: empty result rather than hard error so startup
		// probes do not surface as MCP client failures.
		result = map[string]any{}
	}

	respondJSON(w, map[string]any{
		"jsonrpc": "2.0",
		"id":      json.RawMessage(req.ID),
		"result":  result,
	})
}

// checkAdminAuth accepts an operator session or the break-glass admin token.
// Session requests that change state must also pass the Passport CSRF check.
func (h *proxyHandler) checkAdminAuth(w http.ResponseWriter, r *http.Request) bool {
	// The explicit break-glass credential takes precedence over any browser
	// cookie, including a stale or lower-role Passport session. A banned peer
	// is refused before the comparison, otherwise the ban would not stop it
	// from guessing the token.
	if h.cfg != nil && h.cfg.adminToken != "" && r.Header.Get("X-Admin-Token") != "" {
		ip := getClientIP(r)
		if h.bruteForce != nil && h.bruteForce.isBanned(ip) {
			respondJSONError(w, http.StatusTooManyRequests, "too many failed attempts, try again later")
			return false
		}
		if secureSecretEquals(r.Header.Get("X-Admin-Token"), h.cfg.adminToken) {
			if h.bruteForce != nil {
				h.bruteForce.recordSuccess(ip)
			}
			*r = *r.WithContext(context.WithValue(r.Context(), providerContributionActorKey{}, "break-glass"))
			return true
		}
	}
	if h.passport != nil {
		if principal, session := h.passport.authenticate(r); principal != nil {
			if principal.Kind != PrincipalOperator {
				respondJSONError(w, http.StatusForbidden, "forbidden")
				return false
			}
			if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions && !h.passportCSRF(r, session) {
				respondJSONError(w, http.StatusForbidden, "invalid CSRF token")
				return false
			}
			*r = *r.WithContext(context.WithValue(r.Context(), providerContributionActorKey{}, principal.ID))
			return true
		}
	}
	ip := getClientIP(r)
	if h.bruteForce != nil && h.bruteForce.isBanned(ip) {
		respondJSONError(w, http.StatusTooManyRequests, "too many failed attempts, try again later")
		return false
	}

	if h.cfg.adminToken == "" {
		// No admin token configured - deny all admin access
		log.Printf("admin auth: no token configured")
		respondJSONError(w, http.StatusForbidden, "admin access disabled")
		return false
	}

	// Secrets belong in headers, never URLs or logs.
	token := r.Header.Get("X-Admin-Token")

	if h.cfg.debug.Load() {
		log.Printf("admin auth: credential_present=%v", token != "")
	}

	if h.bruteForce != nil {
		h.bruteForce.recordFailure(ip)
	}
	respondJSONError(w, http.StatusUnauthorized, "invalid admin token")
	return false
}

// checkMemberOrAdminAuth permits a live member/operator session or the
// break-glass admin token. The retired friend code is never an authority input.
func (h *proxyHandler) checkMemberOrAdminAuth(w http.ResponseWriter, r *http.Request) bool {
	if h.passport != nil {
		if principal, _ := h.passport.authenticate(r); principal != nil {
			if principal.Kind == PrincipalMember || principal.Kind == PrincipalOperator {
				return true
			}
			http.Error(w, "forbidden", http.StatusForbidden)
			return false
		}
	}
	ip := getClientIP(r)
	if h.bruteForce != nil && h.bruteForce.isBanned(ip) {
		http.Error(w, "too many failed attempts, try again later", http.StatusTooManyRequests)
		return false
	}
	if h.cfg.adminToken != "" && secureSecretEquals(r.Header.Get("X-Admin-Token"), h.cfg.adminToken) {
		if h.bruteForce != nil {
			h.bruteForce.recordSuccess(ip)
		}
		return true
	}
	if h.bruteForce != nil {
		h.bruteForce.recordFailure(ip)
	}
	http.Error(w, "unauthorized", http.StatusUnauthorized)
	return false
}

type providerContributionActorKey struct{}

func (h *proxyHandler) checkProviderContributionAuth(w http.ResponseWriter, r *http.Request) bool {
	if h.cfg != nil && h.cfg.adminToken != "" && secureSecretEquals(r.Header.Get("X-Admin-Token"), h.cfg.adminToken) {
		if !h.checkAdminAuth(w, r) {
			return false
		}
		*r = *r.WithContext(context.WithValue(r.Context(), providerContributionActorKey{}, "break-glass"))
		return true
	}
	if h.passport != nil {
		if principal, session := h.passport.authenticate(r); principal != nil {
			if principal.Kind != PrincipalOperator && (principal.Kind != PrincipalMember || !principal.CanContribute) {
				http.Error(w, "forbidden", http.StatusForbidden)
				return false
			}
			if !h.passportCSRF(r, session) {
				respondJSONError(w, http.StatusForbidden, "invalid CSRF token")
				return false
			}
			*r = *r.WithContext(context.WithValue(r.Context(), providerContributionActorKey{}, principal.ID))
			return true
		}
	}
	if !h.checkAdminAuth(w, r) {
		return false
	}
	*r = *r.WithContext(context.WithValue(r.Context(), providerContributionActorKey{}, "break-glass"))
	return true
}

func providerContributionActor(r *http.Request) string {
	actor, _ := r.Context().Value(providerContributionActorKey{}).(string)
	return actor
}

// localHealthProbe permits only read-only health routes from a direct local
// peer whose forwarded client identity is also local.
func localHealthProbe(r *http.Request) bool {
	if r.Method != http.MethodGet && r.Method != http.MethodHead {
		return false
	}
	switch r.URL.Path {
	case "/healthz", "/healthz/", "/livez", "/livez/", "/readyz", "/readyz/":
	default:
		return false
	}
	host, _, err := net.SplitHostPort(r.RemoteAddr)
	if err != nil {
		return false
	}
	peer, client := net.ParseIP(host), net.ParseIP(getClientIP(r))
	return peer != nil && client != nil && peer.IsLoopback() && client.IsLoopback()
}

func (p *ipAccessPolicy) allowRequest(w http.ResponseWriter, r *http.Request) bool {
	clientIP, err := requestClientIP(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return false
	}
	if p.restricted() && !localHealthProbe(r) && !p.permitted(clientIP) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return false
	}
	return true
}

func (h *proxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.httpAppOnce.Do(func() {
		h.httpApp = h.newHTTPApp(globalIPAccess)
	})
	h.httpApp.ServeHTTP(w, r)
}

func (h *proxyHandler) handleRouteTrace(w http.ResponseWriter, r *http.Request) {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	reqID := strings.TrimPrefix(r.URL.Path, "/api/pool/routes/")
	reqID = strings.TrimSpace(reqID)
	if reqID == "" {
		respondJSONError(w, http.StatusBadRequest, "missing request_id")
		return
	}

	traces := h.routeTraces
	if traces == nil {
		respondJSONError(w, http.StatusServiceUnavailable, "route trace store is not initialized")
		return
	}
	trace, ok := traces.Get(reqID)
	if !ok {
		respondJSONError(w, http.StatusNotFound, "route trace not found")
		return
	}

	isOperator := h.isOperatorRequest(r)
	if isOperator {
		respondJSON(w, trace)
	} else {
		respondJSON(w, traces.SanitizeForClient(trace))
	}
}

func (h *proxyHandler) isOperatorRequest(r *http.Request) bool {
	if h.passport != nil {
		if principal, _ := h.passport.authenticate(r); principal != nil && principal.Kind == PrincipalOperator {
			return true
		}
	}
	if h.cfg.adminToken != "" && secureSecretEquals(r.Header.Get("X-Admin-Token"), h.cfg.adminToken) {
		return true
	}
	return false
}
