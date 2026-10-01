package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net"
	"net/http"
	httppprof "net/http/pprof"
	"sort"
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
	// cookie, including a stale or lower-role Passport session.
	if h.cfg != nil && h.cfg.adminToken != "" && secureSecretEquals(r.Header.Get("X-Admin-Token"), h.cfg.adminToken) {
		if h.bruteForce != nil {
			h.bruteForce.recordSuccess(getClientIP(r))
		}
		*r = *r.WithContext(context.WithValue(r.Context(), providerContributionActorKey{}, "break-glass"))
		return true
	}
	if h.passport != nil {
		if principal, session := h.passport.authenticate(r); principal != nil {
			if principal.Kind != PrincipalOperator {
				http.Error(w, "forbidden", http.StatusForbidden)
				return false
			}
			if r.Method != http.MethodGet && r.Method != http.MethodHead && r.Method != http.MethodOptions && !h.passportCSRF(r, session) {
				http.Error(w, "invalid CSRF token", http.StatusForbidden)
				return false
			}
			*r = *r.WithContext(context.WithValue(r.Context(), providerContributionActorKey{}, principal.ID))
			return true
		}
	}
	ip := getClientIP(r)
	if h.bruteForce != nil && h.bruteForce.isBanned(ip) {
		http.Error(w, "too many failed attempts, try again later", http.StatusTooManyRequests)
		return false
	}

	if h.cfg.adminToken == "" {
		// No admin token configured - deny all admin access
		log.Printf("admin auth: no token configured")
		http.Error(w, "admin access disabled", http.StatusForbidden)
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
	http.Error(w, "unauthorized", http.StatusUnauthorized)
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

// ServeHTTP routes incoming requests to the appropriate handler.
func (h *proxyHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	applySecurityHeaders(w, r.URL.Path)
	clientIP, err := requestClientIP(r)
	if err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	}
	if globalIPAccess.restricted() && !localHealthProbe(r) && !globalIPAccess.permitted(clientIP) {
		http.Error(w, "forbidden", http.StatusForbidden)
		return
	}
	reqID := strings.TrimSpace(r.Header.Get("X-Request-ID"))
	if reqID == "" {
		reqID = strings.TrimSpace(r.Header.Get("x-request-id"))
	}
	if reqID == "" {
		reqID = randomID()
	}
	w.Header().Set("X-Pool-Request-Id", reqID)
	if r.URL.Path == "/admin/debug/transition" {
		h.handleTransitionDryRun(w, r)
		return
	}
	if r.URL.Path == "/admin/transitions" || strings.HasPrefix(r.URL.Path, "/admin/debug/conversations/") {
		if !h.checkAdminAuth(w, r) {
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if r.URL.Path == "/admin/transitions" {
			events := h.getContextHandoff().TransitionDiagnostics(conversationKey{})
			sort.Slice(events, func(i, j int) bool { return events[i].Timestamp.After(events[j].Timestamp) })
			if len(events) > 100 {
				events = events[:100]
			}
			respondJSON(w, events)
			return
		}
		id := strings.TrimPrefix(r.URL.Path, "/admin/debug/conversations/")
		if id == "" || strings.Contains(id, "/") {
			respondJSONError(w, http.StatusBadRequest, "invalid conversation id")
			return
		}
		key, matches := h.getContextHandoff().resolveConversationKey("", id)
		if matches == 0 {
			respondJSONError(w, http.StatusNotFound, "conversation not found")
			return
		}
		if matches > 1 {
			respondJSONError(w, http.StatusConflict, fmt.Sprintf("conversation id shared by %d owners", matches))
			return
		}
		state, _ := h.getContextHandoff().State(key)
		respondJSON(w, map[string]any{"conversation_id": id, "owner": key.owner, "active_provider": state.ActiveProvider,
			"transition_epoch": state.TransitionEpoch, "transitions": h.getContextHandoff().TransitionDiagnostics(key)})
		return
	}
	if h.cfg != nil && h.cfg.debug.Load() {
		log.Printf("[%s] incoming %s %s", reqID, r.Method, r.URL.Path)
	}

	// Profiles may contain request data and runtime internals; require admin auth.
	if strings.HasPrefix(r.URL.Path, "/debug/pprof/") {
		if !h.checkAdminAuth(w, r) {
			return
		}
		switch r.URL.Path {
		case "/debug/pprof/profile":
			httppprof.Profile(w, r)
		case "/debug/pprof/trace":
			httppprof.Trace(w, r)
		case "/debug/pprof/cmdline":
			httppprof.Cmdline(w, r)
		case "/debug/pprof/symbol":
			httppprof.Symbol(w, r)
		default:
			httppprof.Index(w, r)
		}
		return
	}

	// Fingerprinted signal-room assets are embedded by the Go binary.
	if strings.HasPrefix(r.URL.Path, "/assets/") {
		h.serveSignalRoomAsset(w, r)
		return
	}

	// Static routes
	switch r.URL.Path {
	case "/":
		h.servePassportSPA(w, r)
		return
	case "/join", "/recover":
		h.servePassportSPA(w, r)
		return
	case "/cute-code":
		h.serveCuteCodeLanding(w, r)
		return
	case "/status":
		h.serveStatusPage(w, r)
		return
	case "/og-image.png":
		h.serveOGImage(w, r)
		return
	case "/hero.png", "/hero.webp":
		h.serveHeroImage(w, r)
		return
	case "/api/auth/login":
		h.handlePassportLogin(w, r)
		return
	case "/api/auth/config":
		h.handleAuthConfig(w, r)
		return
	case "/api/auth/join":
		h.handleJoin(w, r)
		return
	case "/api/auth/recover/status":
		h.handleMemberRecoveryStatus(w, r)
		return
	case "/api/auth/recover":
		h.handleMemberRecovery(w, r)
		return
	case "/api/auth/me":
		h.handlePassportMe(w, r)
		return
	case "/api/auth/logout":
		h.handlePassportLogout(w, r)
		return
	case "/api/auth/passkey/begin":
		h.handleWebAuthnLoginBegin(w, r)
		return
	case "/api/auth/passkey/finish":
		h.handleWebAuthnLoginFinish(w, r)
		return
	case "/api/me/passkeys":
		h.handlePasskeys(w, r)
		return
	case "/api/me/passkeys/register/begin":
		h.handleWebAuthnRegisterBegin(w, r)
		return
	case "/api/me/passkeys/register/finish":
		h.handleWebAuthnRegisterFinish(w, r)
		return
	case "/api/me/profile":
		h.handlePassportProfile(w, r)
		return
	case "/api/me/avatar":
		h.handlePassportAvatarUpload(w, r)
		return
	case "/api/passes":
		h.handlePasses(w, r)
		return
	case "/api/me/accounts":
		h.handleMyAccounts(w, r)
		return
	case "/api/me/clients":
		h.handlePassportClients(w, r)
		return
	case "/api/me/usage":
		h.handlePassportUsage(w, r)
		return
	case "/api/console/principals":
		h.handleConsolePrincipals(w, r)
		return
	case "/api/console/members":
		h.handleConsoleMembers(w, r)
		return
	case "/api/console/audit":
		h.handleConsoleAudit(w, r)
		return
	case "/api/console/analytics-health":
		h.handleConsoleAnalyticsHealth(w, r)
		return
	case "/api/setup/operator":
		h.handleOperatorBootstrap(w, r)
		return
	case "/api/pool/stats":
		if !h.checkAdminAuth(w, r) {
			return
		}
		h.handlePoolStats(w, r)
		return
	case "/api/pool/performance":
		if !h.checkAdminAuth(w, r) {
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		summary := h.metrics.performanceSummary(h.pool)
		respondJSON(w, summary)
		return
	case "/api/pool/circuit-breakers":
		if !h.checkAdminAuth(w, r) {
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		respondJSON(w, h.getCircuitBreakers().Snapshot())
		return
	case "/api/pool/whoami":
		h.handleWhoami(w, r)
		return
	case "/api/pool/users":
		if !h.checkAdminAuth(w, r) {
			return
		}
		h.handlePoolUsers(w, r)
		return
	case "/api/pool/origins":
		if !h.checkAdminAuth(w, r) {
			return
		}
		h.handlePoolOrigins(w, r)
		return
	case "/api/pool/daily-breakdown":
		if !h.checkAdminAuth(w, r) {
			return
		}
		h.handleDailyBreakdown(w, r)
		return
	case "/api/pool/hourly":
		if !h.checkAdminAuth(w, r) {
			return
		}
		h.handleGlobalHourly(w, r)
		return
	case "/api/pool/signal":
		if !h.checkAdminAuth(w, r) {
			return
		}
		h.handleSignalAnalytics(w, r)
		return
	case "/api/console/principal-budget":
		h.handlePrincipalBudget(w, r)
		return
	case "/api/console/account-contribution":
		h.handleContributionPolicy(w, r)
		return
	case "/api/pool/catalog":
		if !h.checkMemberOrAdminAuth(w, r) {
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if h.isOperatorRequest(r) {
			servePoolModels(w, h.requestVisiblePool(r))
		} else {
			serveClientPoolModels(w, h.requestVisiblePool(r))
		}
		return
	case "/api/pool/experiments":
		if !h.checkAdminAuth(w, r) {
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.serveExperimentMetrics(w)
		return
	case "/favicon.ico", "/favicon.png":
		h.serveFavicon(w, r)
		return
	case "/healthz", "/healthz/":
		h.serveHealth(w)
		return
	case "/livez", "/livez/":
		h.serveLivez(w)
		return
	case "/readyz", "/readyz/":
		h.serveReadyz(w)
		return
	case "/metrics":
		if !h.checkAdminAuth(w, r) {
			return
		}
		h.serveOperationalMetrics(w, r)
		return
	case "/admin/reload":
		if !h.checkAdminAuth(w, r) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.reloadAccounts()
		w.WriteHeader(http.StatusOK)
		w.Write([]byte("ok"))
		return
	case "/admin/accounts":
		if !h.checkAdminAuth(w, r) {
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.serveAccounts(w, r)
		return
	case "/admin/origins":
		if !h.checkAdminAuth(w, r) {
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.handleAdminOrigins(w, r)
		return
	case "/admin/tokens":
		if !h.checkAdminAuth(w, r) {
			return
		}
		if r.Method != http.MethodGet {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.serveTokenCapacity(w)
		return
	case "/admin/clear-rate-limits":
		if !h.checkAdminAuth(w, r) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.clearAllRateLimits(w)
		return
	case "/admin/purge-anonymous":
		if !h.checkAdminAuth(w, r) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		h.purgeAnonymousUsers(w)
		return
	}

	// Account enable/disable: /admin/accounts/:id/{enable,disable}
	if strings.HasPrefix(r.URL.Path, "/admin/accounts/") &&
		(strings.HasSuffix(r.URL.Path, "/enable") || strings.HasSuffix(r.URL.Path, "/disable")) {
		if !h.checkAdminAuth(w, r) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/admin/accounts/")
		disabled := strings.HasSuffix(path, "/disable")
		accountID := strings.TrimSuffix(strings.TrimSuffix(path, "/disable"), "/enable")
		if !h.authorizeAccountManagement(w, r, accountID) { return }
		h.setAccountDisabled(w, accountID, disabled)
		return
	}

	// Account resurrect: /admin/accounts/:id/resurrect
	if strings.HasPrefix(r.URL.Path, "/admin/accounts/") && strings.HasSuffix(r.URL.Path, "/resurrect") {
		if !h.checkAdminAuth(w, r) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		// Extract account ID from path
		path := strings.TrimPrefix(r.URL.Path, "/admin/accounts/")
		accountID := strings.TrimSuffix(path, "/resurrect")
		if !h.authorizeAccountManagement(w, r, accountID) { return }
		h.resurrectAccount(w, accountID)
		return
	}

	// Account force refresh: /admin/accounts/:id/refresh
	if strings.HasPrefix(r.URL.Path, "/admin/accounts/") && strings.HasSuffix(r.URL.Path, "/refresh") {
		if !h.checkAdminAuth(w, r) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		path := strings.TrimPrefix(r.URL.Path, "/admin/accounts/")
		accountID := strings.TrimSuffix(path, "/refresh")
		if !h.authorizeAccountManagement(w, r, accountID) { return }
		h.forceRefreshAccount(w, accountID)
		return
	}

	// Route Trace: /api/pool/routes/:request_id
	if strings.HasPrefix(r.URL.Path, "/api/pool/routes/") {
		if !h.checkAdminAuth(w, r) {
			return
		}
		h.handleRouteTrace(w, r)
		return
	}

	// User daily usage: /api/pool/users/:id/daily
	if strings.HasPrefix(r.URL.Path, "/api/pool/users/") && strings.HasSuffix(r.URL.Path, "/daily") {
		if !h.checkAdminAuth(w, r) {
			return
		}
		h.handleUserDaily(w, r)
		return
	}

	// User hourly usage: /api/pool/users/:id/hourly
	if strings.HasPrefix(r.URL.Path, "/api/pool/users/") && strings.HasSuffix(r.URL.Path, "/hourly") {
		if !h.checkAdminAuth(w, r) {
			return
		}
		h.handleUserHourly(w, r)
		return
	}

	// Setup scripts
	if strings.HasPrefix(r.URL.Path, "/setup/codex/") {
		h.serveCodexSetupScript(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/setup/gemini/") {
		h.serveGeminiSetupScript(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/setup/antigravity/") {
		h.serveAntigravitySetupScript(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/setup/claude/") {
		h.serveClaudeSetupScript(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/setup/cute-code/") {
		h.serveCuteCodeSetupScript(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/setup/grok/") {
		h.serveGrokSetupScript(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/setup/pi/") {
		h.servePiSetupScript(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/config/cute-code/") {
		h.serveCuteCodeSettingsConfig(w, r)
		return
	}

	if strings.HasPrefix(r.URL.Path, "/api/pool/accounts/") {
		if !h.checkProviderContributionAuth(w, r) {
			return
		}
		if r.Method != http.MethodPost {
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
		switch r.URL.Path {
		case "/api/pool/accounts/codex/add":
			h.handleCodexAdd(w, r)
		case "/api/pool/accounts/codex/exchange":
			h.handleCodexExchange(w, r)
		case "/api/pool/accounts/claude/add":
			h.handleClaudeAdd(w, r)
		case "/api/pool/accounts/claude/exchange":
			h.handleClaudeExchange(w, r)
		case "/api/pool/accounts/antigravity/add":
			h.handleAntigravityAdd(w, r)
		case "/api/pool/accounts/antigravity/status":
			h.handleAntigravityStatus(w, r)
		case "/api/pool/accounts/antigravity/exchange":
			h.handleAntigravityExchange(w, r)
		case "/api/pool/accounts/kimi/add":
			h.handleKimiAdd(w, r)
		case "/api/pool/accounts/minimax/add":
			h.handleMinimaxAdd(w, r)
		case "/api/pool/accounts/zai/add":
			h.handleZAIAdd(w, r)
		case "/api/pool/accounts/zai/login/init":
			h.handleZAILoginInit(w, r)
		case "/api/pool/accounts/zai/login/poll":
			h.handleZAILoginPoll(w, r)
		case "/api/pool/accounts/xiaomi/add":
			h.handleXiaomiAdd(w, r)
		case "/api/pool/accounts/grok/add":
			h.handleGrokImport(w, r)
		case "/api/pool/accounts/opencode-go/add":
			h.handleOpencodeGoAdd(w, r)
		default:
			http.NotFound(w, r)
		}
		return
	}

	// Provider mutations require operator authority. The Claude OAuth
	// callback remains public because it is invoked by the upstream redirect;
	// exchanging that callback for credentials still requires admin auth.
	if strings.HasPrefix(r.URL.Path, "/admin/claude") {
		if r.URL.Path != "/admin/claude/callback" && !h.checkAdminAuth(w, r) {
			return
		}
		h.serveClaudeAdmin(w, r)
		return
	}

	if strings.HasPrefix(r.URL.Path, "/admin/codex") {
		if !h.checkAdminAuth(w, r) {
			return
		}
		h.serveCodexAdmin(w, r)
		return
	}

	if strings.HasPrefix(r.URL.Path, "/admin/antigravity") {
		if r.URL.Path == "/admin/antigravity/callback" {
			if r.Method != http.MethodGet {
				http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
				return
			}
			h.handleAntigravityCallback(w, r)
			return
		}
		if !h.checkAdminAuth(w, r) {
			return
		}
		if r.URL.Path == "/admin/antigravity/relogin" && r.Method == http.MethodPost {
			h.handleAntigravityRelogin(w, r)
			return
		}
		if r.URL.Path == "/admin/antigravity/exchange" && r.Method == http.MethodPost {
			h.handleAntigravityExchange(w, r)
			return
		}
		if r.URL.Path == "/admin/antigravity/models/sync" && r.Method == http.MethodPost {
			h.handleAntigravityModelSync(w, r)
			return
		}
		if r.URL.Path == "/admin/antigravity/models/verify" && r.Method == http.MethodPost {
			h.handleAntigravityModelVerify(w, r)
			return
		}
		http.NotFound(w, r)
		return
	}

	if strings.HasPrefix(r.URL.Path, "/admin/kimi") {
		if !h.checkAdminAuth(w, r) {
			return
		}
		h.serveKimiAdmin(w, r)
		return
	}

	if strings.HasPrefix(r.URL.Path, "/admin/minimax") {
		if !h.checkAdminAuth(w, r) {
			return
		}
		h.serveMinimaxAdmin(w, r)
		return
	}

	if strings.HasPrefix(r.URL.Path, "/admin/zai") {
		if !h.checkAdminAuth(w, r) {
			return
		}
		h.serveZAIAdmin(w, r)
		return
	}

	if strings.HasPrefix(r.URL.Path, "/admin/xiaomi") {
		if !h.checkAdminAuth(w, r) {
			return
		}
		h.serveXiaomiAdmin(w, r)
		return
	}

	if strings.HasPrefix(r.URL.Path, "/admin/adverserial") {
		if !h.checkAdminAuth(w, r) {
			return
		}
		h.serveAdverserialAdmin(w, r)
		return
	}

	if strings.HasPrefix(r.URL.Path, "/admin/grok") {
		if !h.checkAdminAuth(w, r) {
			return
		}
		h.serveGrokAdmin(w, r)
		return
	}

	if strings.HasPrefix(r.URL.Path, "/admin/opencode-go") {
		if !h.checkAdminAuth(w, r) {
			return
		}
		h.serveOpencodeGoAdmin(w, r)
		return
	}

	if strings.HasPrefix(r.URL.Path, "/api/avatars/") {
		h.handlePassportAvatar(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/me/passkeys/") {
		h.handlePasskeys(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/account-grants/") {
		if h.passport == nil {
			respondJSONError(w, 503, "account authority unavailable")
			return
		}
		h.handleGrantRevoke(w, r, strings.TrimPrefix(r.URL.Path, "/api/account-grants/"))
		return
	}
	if r.URL.Path == "/api/console/shareable-accounts" {
		h.handleShareableAccounts(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/console/policies/") {
		id, preview, ok := policyEditorPath(r.URL.Path)
		if !ok {
			http.NotFound(w, r)
			return
		}
		h.handlePolicyEditor(w, r, id, preview)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/accounts/") {
		parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/accounts/"), "/")
		if len(parts) == 3 && (parts[2] == "grants" || parts[2] == "delegation") {
			h.handleAccountSharing(w, r, AccountType(parts[0]), parts[1], parts[2])
			return
		}
		if len(parts) == 3 && parts[2] == "controls" {
			h.handleAccountControls(w, r, AccountType(parts[0]), parts[1])
			return
		}
		http.NotFound(w, r)
		return
	}

	if strings.HasPrefix(r.URL.Path, "/api/me/accounts/") {
		h.handleMyAccountItem(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/me/clients/") {
		h.handlePassportClientItem(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/passes/") {
		h.handlePassItem(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/principals/") {
		h.handlePrincipalItem(w, r)
		return
	}
	if strings.HasPrefix(r.URL.Path, "/api/console/principals/") && strings.HasSuffix(r.URL.Path, "/usage") {
		h.handleConsolePrincipalUsage(w, r)
		return
	}

	// Config download routes (no auth - token is the auth)
	if strings.HasPrefix(r.URL.Path, "/config/codex/") || strings.HasPrefix(r.URL.Path, "/config/gemini/") || strings.HasPrefix(r.URL.Path, "/config/antigravity/") || strings.HasPrefix(r.URL.Path, "/config/claude/") || strings.HasPrefix(r.URL.Path, "/config/pi/") || strings.HasPrefix(r.URL.Path, "/config/grok/") {
		h.serveConfigDownload(w, r)
		return
	}

	// Fake refresh handler so Codex CLI never needs to hit the real auth server.
	if strings.HasPrefix(r.URL.Path, "/oauth/token") {
		h.serveFakeOAuthToken(w, r)
		return
	}

	// Pool users never own reset credits. Redemption is reserved for the
	// server-side account poller, which calls ChatGPT directly.
	if isCodexResetCreditsPath(r.URL.Path) {
		servePoolCodexResetCredits(w, r)
		return
	}

	// CLI-local responses use the same live principal/client authorization as proxy traffic.
	if isUsageRequest(r) {
		if !h.requirePoolCredential(w, r) {
			return
		}
		h.serveClientCodexUsage(w)
		return
	}

	if isClaudeProfileRequest(r) {
		if !h.requirePoolCredential(w, r) {
			return
		}
		h.serveClientClaudeProfile(w)
		return
	}
	if isClaudeUsageRequest(r) {
		if !h.requirePoolCredential(w, r) {
			return
		}
		h.serveClientClaudeUsage(w)
		return
	}

	if shouldNoopCodexPath(r.URL.Path) {
		serveNoopCodexPath(w, r)
		return
	}

	// Default: proxy to upstream
	h.proxyRequest(w, r, reqID)
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

	traces := h.getRouteTraces()
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
