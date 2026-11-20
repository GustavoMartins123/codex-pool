package main

import (
	"log/slog"
	"net/http"
	"strings"
	"time"

	"codex-pool-proxy/internal/httpapi"
	"go.etcd.io/bbolt"
)

type httpRouteRegister func(string, http.HandlerFunc)

type proxyRoutes struct {
	*proxyHandler
	runtime proxyRuntime
}

// subtree registers the bare path explicitly because ServeMux otherwise
// redirects it to the slash-terminated subtree.
func (register httpRouteRegister) subtree(path string, root, children http.HandlerFunc) {
	register(path, root)
	register(path+"/", children)
}

func (h *proxyHandler) httpHandler() *httpapi.App {
	h.httpAppOnce.Do(func() {
		routes := &proxyRoutes{proxyHandler: h, runtime: systemProxyRuntime()}
		h.httpApp = routes.newHTTPApp(globalIPAccess)
	})
	return h.httpApp
}

func (h *proxyRoutes) newHTTPApp(ipAccess *ipAccessPolicy) *httpapi.App {
	app := httpapi.New(applySecurityHeaders, ipAccess.allowRequest, randomID, h.logIncomingRequest(h.serveGatewayTraffic))

	early := httpRouteRegister(app.HandleFunc)
	early("/api/runner/catalog", h.handleRunnerCatalog)
	early("/api/me/runner-catalog", h.handleAppRunnerCatalog)
	early("/api/me/runner-catalog/events", h.handleAppRunnerCatalog)
	early("/api/runner/ws", h.handleRunnerWS)
	early.subtree("/api/runner/pairings", h.handleRunnerPairing, h.handleRunnerPairing)
	early.subtree("/api/me/runner-pairings", h.handleMyRunnerPairings, h.handleAppRunnerPairing)
	early.subtree("/api/me/runners", h.handleMyRunners, h.handleMyRunners)
	early.subtree("/api/conversations", h.handleConversations, h.handleConversations)
	early.subtree("/api/sessions", h.handleSessions, h.handleSessions)
	early("/admin/debug/transition", h.handleTransitionDryRun)
	early("/admin/transitions", h.adminRoute(http.MethodGet, h.handleTransitionDiagnostics))
	early.subtree("/admin/debug/conversations", h.logIncomingRequest(h.serveGatewayTraffic),
		h.adminRoute(http.MethodGet, h.handleConversationDiagnostics))

	register := func(path string, handler http.HandlerFunc) {
		app.HandleFunc(path, h.logIncomingRequest(handler))
	}
	h.registerPassportRoutes(register)
	h.registerPoolRoutes(register)
	h.registerAdminRoutes(register)
	h.registerSetupRoutes(register)
	return app
}

func (h *proxyRoutes) logIncomingRequest(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if h.cfg != nil && h.cfg.debug.Load() {
			h.runtime.logRequest(r.Context(), w.Header().Get(httpapi.RequestIDHeader), "incoming request",
				slog.String("method", r.Method), slog.String("path", r.URL.Path))
		}
		next(w, r)
	}
}

func (h *proxyRoutes) registerPassportRoutes(register httpRouteRegister) {
	register("/{$}", h.servePassportSPA)
	register("/join", h.servePassportSPA)
	register("/recover", h.servePassportSPA)
	register("/cute-code", h.serveCuteCodeLanding)
	register("/status", h.serveStatusPage)
	register("/og-image.png", h.serveOGImage)
	register("/hero.png", h.serveHeroImage)
	register("/hero.webp", h.serveHeroImage)
	register("/favicon.ico", h.serveFavicon)
	register("/favicon.png", h.serveFavicon)
	register.subtree("/assets", h.serveGatewayTraffic, h.serveSignalRoomAsset)

	register("/api/auth/login", h.handlePassportLogin)
	register("/api/auth/token", h.handleAppToken)
	register("/api/auth/config", h.handleAuthConfig)
	register("/api/auth/join", h.handleJoin)
	register("/api/auth/recover/status", h.handleMemberRecoveryStatus)
	register("/api/auth/recover", h.handleMemberRecovery)
	register("/api/auth/me", h.handlePassportMe)
	register("/api/auth/logout", h.handlePassportLogout)
	register("/api/auth/passkey/begin", h.handleWebAuthnLoginBegin)
	register("/api/auth/passkey/finish", h.handleWebAuthnLoginFinish)
	register.subtree("/api/me/passkeys", h.handlePasskeys, h.handlePasskeys)
	register("/api/me/passkeys/register/begin", h.handleWebAuthnRegisterBegin)
	register("/api/me/passkeys/register/finish", h.handleWebAuthnRegisterFinish)
	register("/api/me/profile", h.handlePassportProfile)
	register("/api/me/avatar", h.handlePassportAvatarUpload)
	register.subtree("/api/avatars", h.serveGatewayTraffic, h.handlePassportAvatar)
	register.subtree("/api/passes", h.handlePasses, h.handlePassItem)
	register.subtree("/api/me/accounts", h.handleMyAccounts, h.handleMyAccountItem)
	register("/api/me/quotas", h.handleMyQuotas)
	register("/api/verification/target", h.handleVerificationTarget)
	register("/api/console/model-verifications", h.handleModelVerification)
	register.subtree("/api/me/clients", h.handlePassportClients, h.handlePassportClientItem)
	register("/api/me/usage", h.handlePassportUsage)
	register("/api/me/usage-export", h.handleMyUsageExport)
	register("/api/console/usage-export", h.handleConsoleUsageExport)
	register.subtree("/api/console/principals", h.handleConsolePrincipals, h.handleConsolePrincipalRoute)
	register("/api/console/members", h.handleConsoleMembers)
	register("/api/console/audit", h.handleConsoleAudit)
	register("/api/console/analytics-health", h.handleConsoleAnalyticsHealth)
	register("/api/setup/operator", h.handleOperatorBootstrap)
	register("/api/console/principal-budget", h.handlePrincipalBudget)
	register("/api/console/account-contribution", h.handleContributionPolicy)
	register("/api/console/operator-accounts", h.handleOperatorAccounts)
	register("/api/console/shareable-accounts", h.handleShareableAccounts)
	register.subtree("/api/console/policies", h.serveGatewayTraffic, h.handlePolicyRoute)
	register.subtree("/api/principals", h.serveGatewayTraffic, h.handlePrincipalItem)

	register.subtree("/api/account-grants", h.serveGatewayTraffic, h.handleGrantRoute)
	register.subtree("/api/accounts", h.serveGatewayTraffic, http.NotFound)
	register("/api/accounts/qwen/{id}/token-plan", func(w http.ResponseWriter, r *http.Request) {
		h.handleQwenTokenPlan(w, r, httpapi.PathSegment(r, "id"))
	})
	register("/api/accounts/{provider}/{id}/operator-access", func(w http.ResponseWriter, r *http.Request) {
		h.handleOperatorAccountAccess(w, r, AccountType(httpapi.PathSegment(r, "provider")), httpapi.PathSegment(r, "id"))
	})
	for _, action := range []string{"grants", "delegation"} {
		register("/api/accounts/{provider}/{id}/"+action, func(w http.ResponseWriter, r *http.Request) {
			h.handleAccountSharing(w, r, AccountType(httpapi.PathSegment(r, "provider")), httpapi.PathSegment(r, "id"), action)
		})
	}
	register("/api/accounts/{provider}/{id}/controls", func(w http.ResponseWriter, r *http.Request) {
		h.handleAccountControls(w, r, AccountType(httpapi.PathSegment(r, "provider")), httpapi.PathSegment(r, "id"))
	})
}

func (h *proxyRoutes) handleConsolePrincipalRoute(w http.ResponseWriter, r *http.Request) {
	if strings.HasSuffix(r.URL.Path, "/usage") {
		h.handleConsolePrincipalUsage(w, r)
		return
	}
	h.serveGatewayTraffic(w, r)
}

func (h *proxyHandler) handlePolicyRoute(w http.ResponseWriter, r *http.Request) {
	id, preview, ok := policyEditorPath(r.URL.Path)
	if !ok {
		http.NotFound(w, r)
		return
	}
	h.handlePolicyEditor(w, r, id, preview)
}

func (h *proxyHandler) handleGrantRoute(w http.ResponseWriter, r *http.Request) {
	if h.passport == nil {
		respondJSONError(w, http.StatusServiceUnavailable, "account authority unavailable")
		return
	}
	h.handleGrantRevoke(w, r, strings.TrimPrefix(r.URL.Path, "/api/account-grants/"))
}

func (h *proxyRoutes) registerPoolRoutes(register httpRouteRegister) {
	admin := func(handler http.HandlerFunc) http.HandlerFunc {
		return httpapi.Require(h.checkAdminAuth, handler)
	}
	adminGet := func(handler http.HandlerFunc) http.HandlerFunc {
		return h.adminRoute(http.MethodGet, handler)
	}
	register("/api/pool/stats", admin(h.handlePoolStats))
	register("/api/pool/performance", adminGet(responseOnly(h.servePoolPerformance)))
	register("/api/pool/circuit-breakers", adminGet(responseOnly(h.servePoolCircuitBreakers)))
	register("/api/pool/whoami", h.handleWhoami)
	register.subtree("/api/pool/users", admin(h.handlePoolUsers), h.handlePoolUserRoute)
	register("/api/pool/origins", admin(h.handlePoolOrigins))
	register("/api/pool/daily-breakdown", admin(h.handleDailyBreakdown))
	register("/api/pool/hourly", admin(h.handleGlobalHourly))
	register("/api/pool/signal", admin(h.handleSignalAnalytics))
	register("/api/pool/catalog", h.handlePoolCatalog)
	register("/api/pool/recent-errors", adminGet(responseOnly(h.serveRecentErrors)))
	register("/api/pool/experiments", adminGet(responseOnly(h.serveExperimentMetrics)))
	register.subtree("/api/pool/routes", h.serveGatewayTraffic, admin(h.handleRouteTrace))

	for _, probe := range []struct {
		path    string
		handler func(http.ResponseWriter)
	}{
		{"/healthz", h.serveHealth},
		{"/livez", h.serveLivez},
		{"/readyz", h.serveReadyz},
	} {
		handler := responseOnly(probe.handler)
		register(probe.path, handler)
		register(probe.path+"/{$}", handler)
	}
	register("/metrics", httpapi.Require(h.checkMetricsAuth, h.serveOperationalMetrics))
	h.registerContributionRoutes(register)
}

func responseOnly(handler func(http.ResponseWriter)) http.HandlerFunc {
	return func(w http.ResponseWriter, _ *http.Request) {
		handler(w)
	}
}

func (h *proxyHandler) servePoolPerformance(w http.ResponseWriter) {
	respondJSON(w, h.metrics.performanceSummary(h.pool))
}

func (h *proxyHandler) servePoolCircuitBreakers(w http.ResponseWriter) {
	respondJSON(w, h.getCircuitBreakers().Snapshot())
}

func (h *proxyRoutes) handlePoolUserRoute(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasSuffix(r.URL.Path, "/daily"):
		if h.checkAdminAuth(w, r) {
			h.handleUserDaily(w, r)
		}
	case strings.HasSuffix(r.URL.Path, "/hourly"):
		if h.checkAdminAuth(w, r) {
			h.handleUserHourly(w, r)
		}
	default:
		h.serveGatewayTraffic(w, r)
	}
}

func (h *proxyHandler) handlePoolCatalog(w http.ResponseWriter, r *http.Request) {
	if len(r.Header.Values("Authorization")) != 0 {
		h.handleAppCatalog(w, r)
		return
	}
	if !h.checkMemberOrAdminAuth(w, r) {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	noStore(w)
	visible := h.requestVisiblePool(r)
	if h.passport != nil {
		var loadErr error
		loadErr = h.passport.db.View(func(tx *bbolt.Tx) error {
			visible.catalogVerifications, loadErr = readModelVerifications(tx, time.Now().UTC())
			return loadErr
		})
		if loadErr != nil {
			respondJSONError(w, http.StatusServiceUnavailable, "model verifications unavailable")
			return
		}
	}
	if h.isOperatorRequest(r) {
		servePoolModels(w, visible)
	} else {
		serveClientPoolModels(w, visible)
	}
}

func (h *proxyRoutes) registerSetupRoutes(register httpRouteRegister) {
	scripts := map[string]http.HandlerFunc{
		"codex":       h.serveCodexSetupScript,
		"gemini":      h.serveGeminiSetupScript,
		"antigravity": h.serveAntigravitySetupScript,
		"claude":      h.serveClaudeSetupScript,
		"cute-code":   h.serveCuteCodeSetupScript,
		"grok":        h.serveGrokSetupScript,
		"pi":          h.servePiSetupScript,
	}
	register("/setup/{provider}", h.serveGatewayTraffic)
	register("/setup/{provider}/", func(w http.ResponseWriter, r *http.Request) {
		if handler, ok := scripts[r.PathValue("provider")]; ok {
			handler(w, r)
			return
		}
		h.serveGatewayTraffic(w, r)
	})
	register("/config/{provider}", h.serveGatewayTraffic)
	register("/config/{provider}/", func(w http.ResponseWriter, r *http.Request) {
		switch r.PathValue("provider") {
		case "cute-code":
			h.serveCuteCodeSettingsConfig(w, r)
		case "codex", "gemini", "antigravity", "claude", "pi", "grok":
			h.serveConfigDownload(w, r)
		default:
			h.serveGatewayTraffic(w, r)
		}
	})
}

func (h *proxyRoutes) serveGatewayTraffic(w http.ResponseWriter, r *http.Request) {
	switch {
	case strings.HasPrefix(r.URL.Path, "/oauth/token"):
		h.serveFakeOAuthToken(w, r)
	case isCodexResetCreditsPath(r.URL.Path):
		servePoolCodexResetCredits(w, r)
	case isUsageRequest(r):
		if h.requirePoolCredential(w, r) {
			h.serveClientCodexUsage(w)
		}
	case isClaudeProfileRequest(r):
		if h.requirePoolCredential(w, r) {
			h.serveClientClaudeProfile(w)
		}
	case isClaudeUsageRequest(r):
		if h.requirePoolCredential(w, r) {
			h.serveClientClaudeUsage(w)
		}
	case shouldNoopCodexPath(r.URL.Path):
		serveNoopCodexPath(w, r)
	default:
		h.proxyRequest(w, r, w.Header().Get(httpapi.RequestIDHeader), h.runtime)
	}
}
