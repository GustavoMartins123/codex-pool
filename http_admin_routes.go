package main

import (
	"fmt"
	"net/http"
	httppprof "net/http/pprof"
	"sort"
	"strings"

	"codex-pool-proxy/internal/httpapi"
)

func (h *proxyHandler) adminRoute(method string, handler http.HandlerFunc) http.HandlerFunc {
	return httpapi.Require(h.checkAdminAuth, httpapi.OnlyMethod(method, handler))
}

func (h *proxyHandler) registerAdminRoutes(register httpRouteRegister) {
	admin := func(handler http.HandlerFunc) http.HandlerFunc {
		return httpapi.Require(h.checkAdminAuth, handler)
	}

	// Profiles can contain request data and are accessible only to operators.
	register.subtree("/debug/pprof", h.serveGatewayTraffic, admin(httppprof.Index))
	register("/debug/pprof/profile", admin(httppprof.Profile))
	register("/debug/pprof/trace", admin(httppprof.Trace))
	register("/debug/pprof/cmdline", admin(httppprof.Cmdline))
	register("/debug/pprof/symbol", admin(httppprof.Symbol))

	register("/admin/reload", h.adminRoute(http.MethodPost, h.handleAdminReload))
	register("/admin/origins", h.adminRoute(http.MethodGet, h.handleAdminOrigins))
	register("/admin/tokens", h.adminRoute(http.MethodGet, responseOnly(h.serveTokenCapacity)))
	register("/admin/clear-rate-limits", h.adminRoute(http.MethodPost, h.clearAllRateLimits))
	register("/admin/purge-anonymous", h.adminRoute(http.MethodPost, h.purgeAnonymousUsers))
	h.registerAdminAccountRoutes(register)

	// OAuth redirects are public. Credential exchange uses operator authority.
	register("/admin/claude/callback", h.serveClaudeAdmin)
	register("/admin/antigravity/callback", h.handleAntigravityCallback)
	register("POST /admin/antigravity/relogin", admin(h.handleAntigravityRelogin))
	register("POST /admin/antigravity/exchange", admin(h.handleAntigravityExchange))
	register("POST /admin/antigravity/models/sync", admin(h.handleAntigravityModelSync))
	register("POST /admin/antigravity/models/verify", admin(h.handleAntigravityModelVerify))

	providers := []struct {
		prefix  string
		handler http.HandlerFunc
	}{
		{"claude", admin(h.serveClaudeAdmin)},
		{"codex", admin(h.serveCodexAdmin)},
		{"antigravity", admin(http.NotFound)},
		{"kimi", admin(h.serveKimiAdmin)},
		{"minimax", admin(h.serveMinimaxAdmin)},
		{"zai", admin(h.serveZAIAdmin)},
		{"xiaomi", admin(h.serveXiaomiAdmin)},
		{"adverserial", admin(h.serveAdverserialAdmin)},
		{"grok", admin(h.serveGrokAdmin)},
		{"opencode-go", admin(h.serveOpencodeGoAdmin)},
		{"qwen", admin(h.serveQwenAdmin)},
		{"aistudio", admin(h.serveAIStudioAdmin)},
	}
	providerAdmin := func(w http.ResponseWriter, r *http.Request) {
		for _, provider := range providers {
			if strings.HasPrefix(r.PathValue("provider"), provider.prefix) {
				provider.handler(w, r)
				return
			}
		}
		h.serveGatewayTraffic(w, r)
	}
	register.subtree("/admin/{provider}", providerAdmin, providerAdmin)
}

func (h *proxyHandler) handleAdminReload(w http.ResponseWriter, r *http.Request) {
	detail := ""
	if err := h.reloadAccounts(); err != nil {
		detail = err.Error()
	} else {
		detail = fmt.Sprintf("%d accounts", h.pool.count())
	}
	h.recordAdminAudit(r, "pool.accounts_reloaded", "", detail)
	w.WriteHeader(http.StatusOK)
	w.Write([]byte("ok"))
}

func (h *proxyHandler) registerAdminAccountRoutes(register httpRouteRegister) {
	register("/admin/accounts", h.adminRoute(http.MethodGet, h.serveAccounts))
	register("/admin/accounts/verify", h.adminRoute(http.MethodPost, h.handleAccountVerifyBatch))

	accountAction := func(
		suffix string,
		authorize func(http.ResponseWriter, *http.Request, string) bool,
		action func(http.ResponseWriter, *http.Request, string),
	) http.HandlerFunc {
		return h.adminRoute(http.MethodPost, func(w http.ResponseWriter, r *http.Request) {
			id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/admin/accounts/"), suffix)
			if authorize(w, r, id) {
				action(w, r, id)
			}
		})
	}
	setDisabled := h.adminRoute(http.MethodPost, h.handleAdminAccountDisabled)
	resetCredit := h.adminRoute(http.MethodPost, func(w http.ResponseWriter, r *http.Request) {
		id := strings.TrimSuffix(strings.TrimPrefix(r.URL.Path, "/admin/accounts/"), "/reset-credit/consume")
		h.consumeAccountResetCredit(w, r, id)
	})
	actions := map[string]http.HandlerFunc{
		"enable":  setDisabled,
		"disable": setDisabled,
		"resurrect": accountAction("/resurrect", h.authorizeAccountManagement, func(w http.ResponseWriter, _ *http.Request, id string) {
			h.resurrectAccount(w, id)
		}),
		"refresh": accountAction("/refresh", h.authorizeAccountManagement, func(w http.ResponseWriter, _ *http.Request, id string) {
			h.forceRefreshAccount(w, id)
		}),
		"delete": accountAction("/delete", h.authorizeAccountDeletion, h.handleAccountDelete),
		"verify": accountAction("/verify", h.authorizeAccountManagement, h.handleAccountVerify),
	}
	register("/admin/accounts/", func(w http.ResponseWriter, r *http.Request) {
		path := r.URL.Path
		if strings.HasSuffix(path, "/reset-credit/consume") {
			resetCredit(w, r)
			return
		}
		if handler, ok := actions[path[strings.LastIndexByte(path, '/')+1:]]; ok {
			handler(w, r)
			return
		}
		h.serveGatewayTraffic(w, r)
	})
}

func (h *proxyHandler) handleAdminAccountDisabled(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/admin/accounts/")
	id := strings.TrimSuffix(strings.TrimSuffix(path, "/disable"), "/enable")
	if h.authorizeAccountManagement(w, r, id) {
		h.setAccountDisabled(w, id, strings.HasSuffix(path, "/disable"))
	}
}

func (h *proxyHandler) registerContributionRoutes(register httpRouteRegister) {
	contribute := func(handler http.HandlerFunc) http.HandlerFunc {
		return httpapi.Require(h.checkProviderContributionAuth, httpapi.OnlyMethod(http.MethodPost, func(w http.ResponseWriter, r *http.Request) {
			r.Body = http.MaxBytesReader(w, r.Body, 2<<20)
			handler(w, r)
		}))
	}
	register.subtree("/api/pool/accounts", h.serveGatewayTraffic, contribute(http.NotFound))
	register("/api/pool/accounts/codex/add", contribute(h.handleCodexAdd))
	register("/api/pool/accounts/codex/exchange", contribute(h.handleCodexExchange))
	register("/api/pool/accounts/claude/add", contribute(h.handleClaudeAdd))
	register("/api/pool/accounts/claude/exchange", contribute(h.handleClaudeExchange))
	register("/api/pool/accounts/antigravity/add", contribute(h.handleAntigravityAdd))
	register("/api/pool/accounts/antigravity/status", contribute(h.handleAntigravityStatus))
	register("/api/pool/accounts/antigravity/exchange", contribute(h.handleAntigravityExchange))
	register("/api/pool/accounts/kimi/add", contribute(h.handleKimiAdd))
	register("/api/pool/accounts/minimax/add", contribute(h.handleMinimaxAdd))
	register("/api/pool/accounts/zai/add", contribute(h.handleZAIAdd))
	register("/api/pool/accounts/zai/login/init", contribute(h.handleZAILoginInit))
	register("/api/pool/accounts/zai/login/poll", contribute(h.handleZAILoginPoll))
	register("/api/pool/accounts/xiaomi/add", contribute(h.handleXiaomiAdd))
	register("/api/pool/accounts/grok/add", contribute(h.handleGrokImport))
	register("/api/pool/accounts/opencode-go/add", contribute(h.handleOpencodeGoAdd))
	register("/api/pool/accounts/qwen/add", contribute(h.handleQwenAdd))
	register("/api/pool/accounts/aistudio/add", contribute(h.handleAIStudioAdd))
	register("/api/pool/accounts/qwen/login/init", contribute(h.handleQwenLoginInit))
	register("/api/pool/accounts/qwen/login/authorize", contribute(h.handleQwenLoginAuthorize))
	register("/api/pool/accounts/qwen/login/complete", contribute(h.handleQwenLoginComplete))
}

func (h *proxyHandler) handleTransitionDiagnostics(w http.ResponseWriter, _ *http.Request) {
	events := h.getContextHandoff().TransitionDiagnostics(conversationKey{})
	sort.Slice(events, func(i, j int) bool { return events[i].Timestamp.After(events[j].Timestamp) })
	if len(events) > 100 {
		events = events[:100]
	}
	respondJSON(w, events)
}

func (h *proxyHandler) handleConversationDiagnostics(w http.ResponseWriter, r *http.Request) {
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
}
