package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"sort"
	"strconv"
	"strings"
	"time"

	"codex-pool-proxy/internal/clientip"
	"codex-pool-proxy/internal/httpwire"
	"codex-pool-proxy/internal/requestjson"
	"codex-pool-proxy/internal/wire"
)

func (h *proxyHandler) proxyRequest(w http.ResponseWriter, r *http.Request, reqID string, rt proxyRuntime) {
	start := rt.now()
	authHeader := r.Header.Get("Authorization")

	// codex_apps MCP and other noop paths must never require a pool token.
	// ServeHTTP already routes these, but keep a guard here in case a future
	// caller reaches proxyRequest with a rewritten / stripped path.
	if shouldNoopCodexPath(r.URL.Path) {
		serveNoopCodexPath(w, r)
		return
	}
	if isCodexResetCreditsPath(r.URL.Path) {
		servePoolCodexResetCredits(w, r)
		return
	}

	// Every pool credential path shares one parser and one live authorization check.
	userID, principalID, clientID, credentialKind, credentialAllowed := h.authorizePoolCredentialRequest(r)
	if credentialKind != "" && !credentialAllowed {
		if isContextRequestPath(r.URL.Path) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusForbidden)
			_, _ = io.WriteString(w, `{"error":{"message":"Pool credential rejected. Sign in again."}}`)
			return
		}
		http.Error(w, "pool credential revoked, expired, or unknown", http.StatusForbidden)
		return
	}
	if userID != "" && h.cfg.debug.Load() {
		rt.logRequest(r.Context(), reqID, "pool user request",
			slog.String("credential_kind", credentialKind), slog.String("user_id", userID))
	}

	// Check if this looks like a real provider credential that should be passed through.
	// When Passport authentication is active, passthrough requests must be authorized
	// by a valid pool credential (e.g. via X-Pool-Token, session cookie, or admin token)
	// so the server is not exploited as an anonymous open egress proxy.
	if isProviderCred, providerType := looksLikeProviderCredential(authHeader); isProviderCred {
		isAuthorized := userID != "" || h.isOperatorOrAdmin(r)
		if !isAuthorized && h.passport != nil {
			if principal, _ := h.passport.authenticate(r); principal != nil && principal.Kind != PrincipalGuest {
				isAuthorized = true
				userID = principal.ID
			}
		}
		if !isAuthorized {
			http.Error(w, "unauthorized: passthrough requests require a valid pool credential", http.StatusUnauthorized)
			return
		}
		if h.cfg.debug.Load() {
			rt.logRequest(r.Context(), reqID, "provider passthrough request", slog.String("provider", string(providerType)))
		}
		h.proxyPassthrough(w, r, reqID, providerType, start, rt)
		return
	}

	// Reject unauthenticated requests - require a valid pool token
	if userID == "" {
		if isContextRequestPath(r.URL.Path) {
			w.Header().Set("Content-Type", "application/json")
			w.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(w, `{"error":{"message":"Sign in with a valid pool credential."}}`)
			return
		}
		http.Error(w, "unauthorized: valid pool token required", http.StatusUnauthorized)
		return
	}
	shadow, internalShadow := trafficShadowFromRequest(r)
	if internalShadow && shadow.principal != userID {
		http.Error(w, "traffic shadow principal mismatch", http.StatusForbidden)
		return
	}
	if r.Method != http.MethodGet && !httpwire.IsWebSocketUpgradeRequest(r) {
		release, err := h.admitQueuedRequest(r, principalID)
		if err != nil {
			respondPolicyError(w, err)
			return
		}
		defer release()
	}
	if isContextRequestPath(r.URL.Path) {
		h.proxyNativeContext(w, r, userID)
		return
	}
	if r.Method == http.MethodGet && normalizeNoopPath(r.URL.Path) == "/api/pool/models" {
		serveClientPoolModels(w, h.catalogPool(userID))
		return
	}
	if r.Method == http.MethodGet && normalizeNoopPath(r.URL.Path) == "/v1/models" {
		serveUnifiedOpenAIModels(w, h.catalogPool(userID))
		return
	}
	if r.Method == http.MethodGet && normalizeNoopPath(r.URL.Path) == "/v1beta/models" {
		serveUnifiedGeminiModels(w, h.catalogPool(userID))
		return
	}
	grantState := newGrantRequestState()
	defer grantState.Release()
	r = r.WithContext(context.WithValue(r.Context(), grantRequestContextKey{}, grantState))
	var admission *policyAdmission
	if h.passport == nil {
		http.Error(w, "passport unavailable", http.StatusServiceUnavailable)
		return
	}
	{
		var candidateAdmission *policyAdmission
		var policyErr error
		if internalShadow {
			candidateAdmission, policyErr = h.passport.beginPolicyRequestReadOnly(principalID, clientID, h.cfg.hotClientPolicies())
		} else {
			candidateAdmission, policyErr = h.passport.beginPolicyRequest(principalID, clientID, h.cfg.hotClientPolicies(), time.Now())
		}
		if policyErr != nil {
			h.auditPolicyDecision(&policyAdmission{principalID: principalID, clientID: clientID}, "policy.request_blocked", policyErr)
			respondPolicyError(w, policyErr)
			return
		}
		admission = candidateAdmission
		if admission != nil {
			defer admission.Release()
			r = r.WithContext(context.WithValue(r.Context(), policyAdmissionContextKey{}, admission))
			if profile := strings.TrimSpace(admission.policy.Routing.Profile); profile != "" {
				r.Header.Set("X-Pool-Routing", profile)
			}
			r.Header.Set("X-Pool-Priority", strconv.Itoa(admission.priority))
			h.auditPolicyDecision(admission, "policy.request_admitted", nil)
		}
	}
	originID := hashRequestOrigin(r, h.originHashSalt())
	originIP := clientip.Get(r)
	if h.store != nil && originID != "" && originIP != "" {
		storedIP := originIP
		if h.cfg.ipPrivacy {
			storedIP = ""
		}
		h.store.enqueueOriginMetadata(originID, storedIP, userID, r.UserAgent(), r.URL.Path, time.Now())
	}

	provider, targetBase := h.pickUpstream(r.URL.Path, r.Header)
	if provider == nil || targetBase == nil {
		http.Error(w, "no upstream for path", http.StatusNotFound)
		return
	}
	accountType := provider.Type()

	if httpwire.IsWebSocketUpgradeRequest(r) {
		if admission.hasTokenBudget() {
			respondPolicyError(w, unboundedPolicyRequest())
			return
		}
		h.proxyRequestWebSocket(w, r, reqID, userID, originID, provider, targetBase, rt)
		return
	}

	streamBody := httpwire.ShouldStreamBody(r, h.cfg.maxInMemoryBodyBytes)
	if streamBody || shouldPeekStreamedModelRoute(r) {
		var err error
		provider, targetBase, err = h.applyStreamedModelRoute(r, provider, targetBase, reqID)
		if err != nil {
			var blocked *policyError
			if errors.As(err, &blocked) {
				h.auditPolicyDecision(admission, "policy.request_blocked", blocked)
				respondPolicyError(w, blocked)
				return
			}
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		accountType = provider.Type()
		streamBody = streamBody || requestPreparationFor(provider).streamOnModelRoute
	}
	preparation := requestPreparationFor(provider)
	// Codex-shaped Responses paths need rewriting and hosted-MCP filtering, but
	// large prompts must not be decoded into map[string]any. Spool token-by-token,
	// then model-route: Grok/Kimi/MiniMax/ZAI/Xiaomi/Adverserial must not inherit
	// the path-selected Codex upstream (especially /backend-api/codex/responses).
	if isCodexResponsesPath(r.URL.Path) && !preparation.bufferResponses {
		largeOrChunked := r.ContentLength < 0 || r.ContentLength > h.cfg.maxInMemoryBodyBytes
		encoding := strings.TrimSpace(strings.ToLower(r.Header.Get("Content-Encoding")))
		uncompressedJSON := encoding == "" || encoding == "identity"
		isCompact := strings.HasSuffix(normalizeNoopPath(r.URL.Path), "/compact")
		if largeOrChunked && uncompressedJSON && !isCompact {
			releaseSpool, err := h.acquireLargeReplayBody(r.Context(), r.ContentLength)
			if err != nil {
				return
			}
			defer releaseSpool()
			rewriteModel := func(model string) string {
				model = strings.TrimSpace(model)
				if resolved, ok := h.aliases.resolve(model); ok {
					model = resolved
				}
				if base, _, hasSuffix := parseThinkingSuffix(model); hasSuffix {
					model = base
				}
				if routed, _, canonical := h.resolveStreamedModelRoute(r.URL.Path, model); routed != nil && canonical != "" {
					return canonical
				}
				return model
			}
			spoolStarted := rt.now()
			spooled, spoolErr := streamCodexResponsesRequest(r.Body, h.cfg.maxSpoolBodyBytes, rewriteModel)
			if spoolErr != nil {
				status := http.StatusBadRequest
				if errors.Is(spoolErr, errResponsesSpoolTooLarge) {
					status = http.StatusRequestEntityTooLarge
				}
				http.Error(w, "streaming Responses rewrite: "+spoolErr.Error(), status)
				return
			}
			defer spooled.Close()
			if spooled.ClientWantsNonStreaming {
				http.Error(w, "oversized Responses requests currently require stream=true", http.StatusBadRequest)
				return
			}
			if spooled.Model != "" {
				if !h.enforcePolicy(w, admission, func() error { return admission.CheckModel(spooled.Model) }) {
					return
				}
				if routed, routeBase, _ := h.resolveStreamedModelRoute(r.URL.Path, spooled.Model); routed != nil {
					provider = routed
					targetBase = routeBase
					accountType = routed.Type()
					if !h.enforcePolicy(w, admission, func() error { return admission.CheckProvider(accountType) }) {
						return
					}
					if h.cfg.debug.Load() {
						log.Printf("[%s] oversized Responses model-route: model=%s provider=%s", reqID, spooled.Model, accountType)
					}
				} else if provider.WireFormat() == WireResponses && !isOpenAIModel(spooled.Model) {
					http.Error(w, fmt.Sprintf("oversized Responses model %q has no routable provider", spooled.Model), http.StatusBadRequest)
					return
				}
			}
			if sanitize := requestPreparationFor(provider).sanitizeSpooled; sanitize != nil {
				if err := sanitize(spooled); err != nil {
					http.Error(w, fmt.Sprintf("%s request sanitizing error: %v", accountType, err), http.StatusBadRequest)
					return
				}
			}
			if provider.WireFormat() == WireResponses {
				r = r.WithContext(context.WithValue(r.Context(), contextSpoolKey{}, spooled))
			}
			r.Body = spooled.File
			// Only the disk-backed body remains; generation must not hold a RAM slot.
			releaseSpool()
			r.ContentLength = spooled.Size
			r.Header.Del("Content-Length")
			r.Header.Set("Content-Type", "application/json")
			log.Printf("[%s] disk-streaming Responses request: bytes=%d model=%s provider=%s transform_ms=%d",
				reqID, spooled.Size, spooled.Model, accountType, rt.now().Sub(spoolStarted).Milliseconds())
			h.proxyRequestStreamed(w, r, reqID, userID, originID, provider, targetBase, rt)
			return
		}
		if provider.WireFormat() == WireResponses {
			streamBody = false
		}
	}
	if preparation.bufferBody || preparation.bufferResponses && isCodexResponsesPath(r.URL.Path) {
		streamBody = false
	}
	if streamBody {
		if h.cfg.debug.Load() {
			log.Printf("[%s] streaming request body: method=%s path=%s provider=%s content-length=%d",
				reqID, r.Method, r.URL.Path, accountType, r.ContentLength)
		}
		h.proxyRequestStreamed(w, r, reqID, userID, originID, provider, targetBase, rt)
		return
	}

	releaseLargeBody, err := h.acquireLargeReplayBody(r.Context(), r.ContentLength)
	if err != nil {
		return
	}
	defer releaseLargeBody()
	bodyBytes, bodySample, err := httpwire.ReadBodyForReplay(r.Body, h.cfg.maxInMemoryBodyBytes, h.cfg.logBodies, h.cfg.bodyLogLimit)
	if err != nil {
		if errors.Is(err, httpwire.ErrReplayBodyTooLarge) {
			http.Error(w, err.Error(), http.StatusRequestEntityTooLarge)
		} else {
			http.Error(w, err.Error(), http.StatusBadRequest)
		}
		return
	}
	if decoded, changed, err := decodeRequestBody(r.Header.Get("Content-Encoding"), bodyBytes, h.cfg.maxInMemoryBodyBytes); err != nil {
		http.Error(w, err.Error(), http.StatusBadRequest)
		return
	} else if changed {
		bodyBytes = decoded
		r.Header.Del("Content-Encoding")
		r.Header.Del("Content-Length")
		r.ContentLength = int64(len(bodyBytes))
	}
	if provider.WireFormat() == WireResponses && isCodexLegacyLivePath(r.URL.Path) {
		bodyBytes, err = rewriteCodexLiveCall(bodyBytes, r.Header.Get("Content-Type"))
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
		r.Header.Set("Content-Type", "application/json")
		r.ContentLength = int64(len(bodyBytes))
	}
	if strings.HasPrefix(r.URL.Path, "/v1/images/generations") {
		if n := wire.ImagesGenerationCount(bodyBytes); n > 1 {
			if h.handleImagesGenerationFanout(w, r, bodyBytes, n, reqID) {
				return
			}
		}
	}

	// conversation_id usually comes from request JSON (Codex often includes it).
	inspect := bodyBytes
	if len(inspect) == 0 {
		inspect = bodySample
	}
	inspect = bodyForInspection(r, inspect)
	var originalObject map[string]any
	originalJSONErr := json.Unmarshal(inspect, &originalObject)
	if originalJSONErr != nil {
		originalObject = nil
	}
	// Capture client intent before normalization forces upstream streaming or
	// removes metadata. Routing and timeout must describe the original request.
	originalModel, _ := originalObject["model"].(string)
	originalModel = strings.TrimSpace(originalModel)
	originalStream, _ := originalObject["stream"].(bool)
	intent := requestIntent{stream: originalStream, image: valueHasImageGenerationTool(originalObject)}
	conversationID := requestjson.ConversationIDFromObject(originalObject)
	if conversationID == "" {
		conversationID = extractConversationIDFromHeaders(r.Header)
	}
	// Use Claude Code session ID as fallback for conversation stickiness
	if conversationID == "" {
		for _, key := range []string{"X-Claude-Code-Session-Id", "x-claude-code-session-id"} {
			if v := strings.TrimSpace(r.Header.Get(key)); v != "" {
				conversationID = v
				break
			}
		}
	}
	// Traffic-experiment legs run in their own conversation namespace: they
	// may never pin, rewrite handoff state, or append assistant history to
	// the production conversation.
	if internalShadow && conversationID != "" {
		conversationID = "shadow:" + shadow.experiment + "\x00" + conversationID
	}
	if h.cfg.debug.Load() && conversationID == "" && originalJSONErr == nil {
		keys := make([]string, 0, len(originalObject))
		for key := range originalObject {
			keys = append(keys, key)
		}
		sort.Strings(keys)
		if len(keys) > 30 {
			keys = keys[:30]
		}
		log.Printf("[%s] conv_id empty; top-level keys (first %d): %s", reqID, len(keys), strings.Join(keys, ","))
	}
	requestedModel := originalModel
	if requestedModel == "" {
		requestedModel = antigravityModelFromGeminiPath(r.URL.Path)
	}

	// Resolve model aliases before routing.
	if requestedModel != "" {
		requestedModel, bodyBytes = applyModelAlias(h.aliases, requestedModel, bodyBytes, h.cfg.debug.Load(), reqID)
	}
	routingProfile, routedModel, routedBody, routingErr := resolveRequestRouting(h.pool, r, requestedModel, bodyBytes)
	if routingErr != nil {
		http.Error(w, routingErr.Error(), http.StatusBadRequest)
		return
	}
	requestedModel, bodyBytes = routedModel, routedBody
	canaryBypass := internalShadow
	if r.Header.Get("X-Pool-Canary-Bypass") != "" {
		if h.isOperatorOrAdmin(r) {
			canaryBypass = true
		}
	}
	var experiment *experimentAssignment
	if h.experiments != nil && !canaryBypass {
		experimentKey := conversationID
		if experimentKey == "" {
			experimentKey = firstNonEmpty(userID, originID, reqID)
		}
		experiment = h.experiments.Assign(requestedModel, experimentKey)
		if experiment != nil && experiment.Model != requestedModel {
			requestedModel = experiment.Model
			if rewritten := requestjson.RewriteModel(bodyBytes, requestedModel); rewritten != nil {
				bodyBytes = rewritten
			}
		}
	}
	if !h.enforcePolicy(w, admission, func() error { return admission.CheckModel(requestedModel) }) {
		return
	}
	if experiment != nil {
		shadowAllowed := admission == nil || admission.CheckModel(experiment.Rule.Candidate) == nil
		if shadowAllowed {
			h.maybeStartShadow(r, bodyBytes, experiment, userID, reqID, rt)
		}
		experimentStarted := time.Now()
		experimentWriter := &experimentResponseWriter{ResponseWriter: w}
		w = experimentWriter
		w.Header().Set("X-Pool-Experiment", experiment.Name)
		w.Header().Set("X-Pool-Variant", experiment.Variant)
		toolRequest := requestUsesTools(bodyBytes)
		defer func() {
			status := experimentWriter.status
			if status == 0 {
				status = http.StatusServiceUnavailable
			}
			ttft := time.Duration(0)
			if !experimentWriter.firstWrite.IsZero() {
				ttft = experimentWriter.firstWrite.Sub(experimentStarted)
			}
			h.experiments.Record(experiment.Name, experiment.Variant, ExperimentObservation{
				Status: status, Duration: time.Since(experimentStarted), TTFT: ttft,
				ResponseBytes: experimentWriter.bytes, StreamError: status >= 500,
				ToolRequest: toolRequest,
			})
		}()
	}

	var autoDecisionInfo *AutoDecision
	if isPoolAutoModel(requestedModel) {
		if h.poolAuto == nil {
			respondJSONError(w, http.StatusServiceUnavailable, "pool/auto is not configured")
			return
		}
		profile := parsePoolAutoProfile(requestedModel)
		reqCaps := extractRequestCapabilities(r.URL.Path, bodyBytes, r.Header)
		var conversation *ConversationState
		if state, ok := h.contextHandoff.State(conversationScopedKey(userID, conversationID)); ok {
			conversation = &state
		}
		dec, err := h.poolAuto.OrchestrateWithTransition(profile, reqCaps, conversationID, h.pool, h.pool.circuitBreakers, h.pricing, h.metrics, conversation)
		if err != nil {
			http.Error(w, "pool/auto orchestration error: "+err.Error(), http.StatusServiceUnavailable)
			return
		}
		autoDecisionInfo = &dec
		requestedModel = dec.SelectedModel
		if !h.enforcePolicy(w, admission, func() error {
			if err := admission.CheckModel(requestedModel); err != nil {
				return err
			}
			return admission.CheckProvider(dec.SelectedProvider)
		}) {
			return
		}
		if rewritten := requestjson.RewriteModel(bodyBytes, requestedModel); rewritten != nil {
			bodyBytes = rewritten
		}
		if h.cfg.debug.Load() {
			log.Printf("[%s] pool/auto (%s) selected model=%s provider=%s score=%.2f",
				reqID, profile, dec.SelectedModel, dec.SelectedProvider, dec.Score)
		}
	}

	// AI Studio keys share the native Gemini conversion with Antigravity but
	// call the public Gemini API, so their prefixed models are routed first.
	if requestedModel != "" && isAIStudioModel(requestedModel) {
		if !h.enforcePolicy(w, admission, func() error { return admission.CheckProvider(AccountTypeAIStudio) }) {
			return
		}
		bodyBytes, err = h.prepareProviderContextHandoff(w, userID, conversationID, AccountTypeAIStudio, r.URL.Path, bodyBytes)
		if err != nil {
			http.Error(w, "context handoff error: "+err.Error(), http.StatusBadRequest)
			return
		}
		if h.handleAIStudioProxy(w, r, bodyBytes, requestedModel, conversationID, userID, originID, originIP, reqID) {
			return
		}
	}

	// Antigravity owns its complete upstream envelope and protocol conversion.
	// Handle it before the generic provider translator so every public protocol
	// consumes the same model registry and quota scheduler.
	if requestedModel != "" && shouldRouteAntigravityModel(h.pool.antigravity, requestedModel) {
		if !h.enforcePolicy(w, admission, func() error { return admission.CheckProvider(AccountTypeAntigravity) }) {
			return
		}
		bodyBytes, err = h.prepareProviderContextHandoff(w, userID, conversationID, AccountTypeAntigravity, r.URL.Path, bodyBytes)
		if err != nil {
			http.Error(w, "context handoff error: "+err.Error(), http.StatusBadRequest)
			return
		}
		if h.handleAntigravityProxy(w, r, bodyBytes, requestedModel, conversationID, userID, originID, originIP, reqID, routingProfile) {
			return
		}
	}

	if requestedModel != "" && isOpenAIModel(requestedModel) {
		var baseModel string
		bodyBytes, baseModel = applyCodexModelSuffixControls(bodyBytes, requestedModel)
		requestedModel = baseModel
		// Applied after suffix controls so a "-xhigh" model name cannot escape
		// the cap. inspect is the routing/analytics view of the body and must
		// stay consistent with what is actually sent upstream.
		if limit := h.effortCap.limitFor(userID, originIP); limit != "" {
			capped, previous, changed := capCodexEffortInBody(bodyBytes, limit)
			if changed {
				bodyBytes = capped
				inspect = bodyForInspection(r, capped)
				if json.Unmarshal(inspect, &originalObject) != nil {
					originalObject = nil
				}
				if r.ContentLength >= 0 {
					r.ContentLength = int64(len(bodyBytes))
					r.Header.Del("Content-Length")
				}
				log.Printf("[%s] effort cap: %s -> %s (user=%s origin=%s model=%s)", reqID, previous, limit, userID, originID, requestedModel)
			}
		}
	}

	// Parse thinking budget suffix before routing (e.g. "claude-sonnet-4-5(16384)").
	// Strip suffix so routing sees the base model name.
	if requestedModel != "" {
		baseName, _, hasSuffix := parseThinkingSuffix(requestedModel)
		if hasSuffix {
			requestedModel = baseName
			// Rewrite model in body to the base name for model routing to work.
			if rewritten := requestjson.RewriteModel(bodyBytes, baseName); rewritten != nil {
				bodyBytes = rewritten
			}
		}
	}

	// Model-based provider override: route to external providers by model name.
	if requestedModel != "" {
		if overrideProvider, overrideBase, rewrittenBody := h.modelRouteOverride(r.URL.Path, requestedModel, bodyBytes); overrideProvider != nil {
			provider = overrideProvider
			targetBase = overrideBase
			accountType = overrideProvider.Type()
			if !h.enforcePolicy(w, admission, func() error { return admission.CheckProvider(accountType) }) {
				return
			}
			if rewrittenBody != nil {
				bodyBytes = rewrittenBody
			}
		}
	}
	if !h.enforcePolicy(w, admission, func() error { return admission.CheckProvider(accountType) }) {
		return
	}

	preparation = requestPreparationFor(provider)
	if setConversationHeaders := preparation.conversationHeaders; setConversationHeaders != nil {
		setConversationHeaders(r, conversationID, userID)
	}

	bodyBytes, err = h.prepareProviderContextHandoff(w, userID, conversationID, accountType, r.URL.Path, bodyBytes)
	if err != nil {
		http.Error(w, "context handoff error: "+err.Error(), http.StatusBadRequest)
		return
	}

	// Inject thinking budget if the original model had a (budget) suffix.
	if requestedModel != "" {
		origModel := originalModel
		if origModel != "" {
			_, budget, hasSuffix := parseThinkingSuffix(origModel)
			if hasSuffix && budget > 0 {
				bodyBytes = injectThinkingBudget(bodyBytes, accountType, budget)
				if h.cfg.debug.Load() {
					log.Printf("[%s] thinking suffix: budget=%d provider=%s", reqID, budget, accountType)
				}
			}
		}
	}

	// --- Format translation: detect mismatch between client format and provider format ---
	sourceFormat := detectRequestFormat(r.URL.Path)
	targetFormat := requestFormatForCodec(provider)
	translateDir := wire.TranslateNone
	var qwenResponses *qwenResponsesRequest
	if accountType == AccountTypeQwen && isCodexResponsesPath(r.URL.Path) {
		qwenResponses, err = prepareQwenResponsesRequest(r.URL.Path, bodyBytes, requestedModel)
		if err != nil {
			respondJSONError(w, http.StatusBadRequest, err.Error())
			return
		}
		bodyBytes = qwenResponses.Body
		r.URL.Path = "/v1/chat/completions"
	}
	if sourceFormat != wire.FormatUnknown && targetFormat != wire.FormatUnknown && sourceFormat != targetFormat {
		if sourceFormat == wire.FormatClaude && targetFormat == wire.FormatOpenAI {
			// Codex backend uses Responses API, not Chat Completions
			if provider.WireFormat() == WireResponses {
				translateDir = wire.TranslateClaudeToResponses
			} else {
				translateDir = wire.TranslateClaudeToOAI
			}
		} else if sourceFormat == wire.FormatOpenAI && targetFormat == wire.FormatClaude {
			translateDir = wire.TranslateOAIToClaude
		}
	}
	// Special case: Chat Completions -> Codex Responses API
	// When client sends /v1/chat/completions and provider is Codex, translate to Responses API format.
	// The Codex upstream (chatgpt.com/backend-api/codex) only speaks Responses API.
	// Codex always requires streaming, so we track whether the client originally wanted non-streaming.
	clientWantsNonStreaming := originalJSONErr == nil && !originalStream
	imagesResponseFormat := ""
	if translateDir == wire.TranslateNone && provider.WireFormat() == WireResponses && (strings.HasPrefix(r.URL.Path, "/v1/images/generations") || strings.HasPrefix(r.URL.Path, "/v1/images/edits")) {
		translateDir = wire.TranslateImagesToResponses
		clientWantsNonStreaming = true
	}
	if translateDir == wire.TranslateNone && sourceFormat == wire.FormatOpenAI && provider.WireFormat() == WireResponses {
		if strings.HasPrefix(r.URL.Path, "/v1/completions") && !strings.HasPrefix(r.URL.Path, "/v1/chat/completions") {
			translateDir = wire.TranslateCompletionsToResponses
		} else {
			translateDir = wire.TranslateChatToResponses
		}
	}
	// Special case: Responses API -> Anthropic Messages API.
	// Route by the provider wire format so Anthropic-compatible providers such as ZAI
	// receive the same tool-call normalization as native Claude.
	if translateDir == wire.TranslateNone && shouldTranslateResponsesToAnthropic(r.URL.Path, targetFormat) {
		translateDir = wire.TranslateResponsesToClaude
	}

	if translateDir != wire.TranslateNone {
		logTranslation(reqID, translateDir, h.cfg.debug.Load())
		var err error
		switch translateDir {
		case wire.TranslateClaudeToOAI:
			bodyBytes, err = wire.AnthropicRequestToOpenAIChat(bodyBytes)
			if err != nil {
				http.Error(w, "format translation error: "+err.Error(), http.StatusBadRequest)
				return
			}
			r.URL.Path = "/v1/chat/completions"
		case wire.TranslateOAIToClaude:
			bodyBytes, err = wire.OpenAIChatRequestToAnthropic(bodyBytes, claudeCanonicalModel)
			if err != nil {
				http.Error(w, "format translation error: "+err.Error(), http.StatusBadRequest)
				return
			}
			r.URL.Path = "/v1/messages"
		case wire.TranslateChatToResponses:
			bodyBytes, err = wire.OpenAIChatRequestToResponses(bodyBytes)
			if err != nil {
				http.Error(w, "format translation error: "+err.Error(), http.StatusBadRequest)
				return
			}
			r.URL.Path = "/v1/responses"
		case wire.TranslateCompletionsToResponses:
			bodyBytes, err = wire.OpenAICompletionsRequestToResponses(bodyBytes)
			if err != nil {
				http.Error(w, "format translation error: "+err.Error(), http.StatusBadRequest)
				return
			}
			r.URL.Path = "/v1/responses"
		case wire.TranslateImagesToResponses:
			if strings.HasPrefix(r.URL.Path, "/v1/images/edits") {
				bodyBytes, imagesResponseFormat, err = wire.ImagesEditRequestToResponses(bodyBytes, r.Header.Get("Content-Type"))
			} else {
				bodyBytes, imagesResponseFormat, err = wire.ImagesGenerationRequestToResponses(bodyBytes)
			}
			if err != nil {
				http.Error(w, "format translation error: "+err.Error(), http.StatusBadRequest)
				return
			}
			r.URL.Path = "/v1/responses"
		case wire.TranslateResponsesToClaude:
			bodyBytes, err = wire.ResponsesRequestToAnthropic(bodyBytes, claudeCanonicalModel)
			if err != nil {
				http.Error(w, "format translation error: "+err.Error(), http.StatusBadRequest)
				return
			}
			r.URL.Path = "/v1/messages"
		case wire.TranslateClaudeToResponses:
			bodyBytes, err = wire.AnthropicRequestToResponses(bodyBytes)
			if err != nil {
				http.Error(w, "format translation error: "+err.Error(), http.StatusBadRequest)
				return
			}
			r.URL.Path = "/v1/responses"
		}
	}
	imageGenerationRequest := false
	compactRequest := provider.WireFormat() == WireResponses && r.Method == http.MethodPost && isCompactPath(r.URL.Path)
	if provider.WireFormat() == WireResponses {
		// Byte-oriented translators retain their contracts. Reuse the original
		// tree only when none of them changed the body; decode changed bytes once.
		upstreamObject := originalObject
		if !bytes.Equal(bodyBytes, inspect) {
			upstreamObject = nil
			if json.Unmarshal(bodyBytes, &upstreamObject) != nil {
				upstreamObject = nil
			}
		}
		if compactRequest && upstreamObject == nil {
			http.Error(w, "compaction request must be a JSON object", http.StatusBadRequest)
			return
		}
		if upstreamObject != nil && isCodexResponsesPath(r.URL.Path) {
			changed := false
			if compactRequest {
				if err := prepareCompactRequest(r, upstreamObject); err != nil {
					http.Error(w, err.Error(), http.StatusBadRequest)
					return
				}
				changed = true
			} else if translateDir == wire.TranslateNone && (strings.HasPrefix(r.URL.Path, "/v1/responses") || strings.HasPrefix(r.URL.Path, "/responses")) {
				prepareResponsesObject(upstreamObject)
				changed = true
			} else {
				changed = stripHostedMCPFromResponsesRequest(upstreamObject)
			}
			// Retain the original envelope for revalidation on each account attempt.
			r = r.WithContext(context.WithValue(r.Context(), contextFrameKey{}, upstreamObject))
			if changed {
				bodyBytes, err = json.Marshal(upstreamObject)
				if err != nil {
					http.Error(w, "hosted MCP request filtering error: "+err.Error(), http.StatusBadRequest)
					return
				}
			}
		}
		imageGenerationRequest = valueHasImageGenerationTool(upstreamObject)
	}

	bodyBytes = wire.NormalizeResponsesSchemaBody(bodyBytes)
	if prepare := preparation.prepareBody; prepare != nil {
		bodyBytes, err = prepare(r.URL.Path, requestedModel, bodyBytes)
		if err != nil {
			http.Error(w, err.Error(), http.StatusBadRequest)
			return
		}
	}

	if h.cfg.debug.Load() {
		rt.logRequest(r.Context(), reqID, "proxy request prepared",
			slog.String("method", r.Method),
			slog.String("path", r.URL.Path),
			slog.String("provider", string(accountType)),
			slog.String("conversation_id", conversationID),
			slog.String("user_id", userID),
			slog.String("origin_id", originID),
			slog.Int("authorization_length", len(r.Header.Get("Authorization"))),
			slog.String("chatgpt_account_id", r.Header.Get("ChatGPT-Account-ID")),
			slog.String("content_type", r.Header.Get("Content-Type")),
			slog.String("content_encoding", r.Header.Get("Content-Encoding")),
			slog.Int("body_bytes", len(bodyBytes)),
			slog.String("model", requestedModel),
		)
	}
	if h.cfg.logBodies && len(bodySample) > 0 && !(provider.WireFormat() == WireResponses && mayHaveContext(bodyBytes)) {
		logSample := bodySample
		if provider.WireFormat() == WireResponses && isCodexResponsesPath(r.URL.Path) {
			logSample = bodyBytes
			if h.cfg.bodyLogLimit > 0 && int64(len(logSample)) > h.cfg.bodyLogLimit {
				logSample = logSample[:h.cfg.bodyLogLimit]
			}
		}
		log.Printf("[%s] request body sample (%d bytes): %s", reqID, len(logSample), safeText(logSample))
	}

	// Determine timeout: honour X-Stainless-Timeout from the Anthropic SDK when present,
	// otherwise fall back to streaming vs non-streaming defaults.
	timeout := timeoutForRequestIntent(r, h.cfg.requestTimeout, h.cfg.streamTimeout, intent)

	ctx := r.Context()
	var cancel context.CancelFunc
	if timeout > 0 {
		ctx, cancel = context.WithTimeout(ctx, timeout)
	} else {
		ctx, cancel = context.WithCancel(ctx)
	}
	defer cancel()

	h.dispatchProxyHTTP(ctx, cancel, w, r, proxyHTTPDispatch{
		body: bodyBytes, baseURL: targetBase, sourceFormat: sourceFormat, targetFormat: targetFormat,
		routingProfile: string(routingProfile), imageGeneration: imageGenerationRequest,
		internalShadow: internalShadow, shadowAccounts: shadow.accounts,
		experiment: experiment, autoDecision: autoDecisionInfo,
		response: proxyHTTPResponse{
			provider: provider, accountType: accountType, admission: admission,
			requestID: reqID, userID: userID, originID: originID, originIP: originIP,
			conversationID: conversationID, model: requestedModel,
			translation: translateDir, imagesResponseFormat: imagesResponseFormat, qwen: qwenResponses,
			nonStreaming: clientWantsNonStreaming, compact: compactRequest,
			runtime: rt, startedAt: start,
		},
	})
}
