package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"

	"codex-pool-proxy/internal/logredact"
	"codex-pool-proxy/internal/routing"
	"codex-pool-proxy/internal/wire"
)

func main() {
	// Before anything can log: the pool has hundreds of log call sites and the
	// stdlib logger offers no per-call redaction hook, so the output stream
	// itself is wrapped. logredact.Install is idempotent.
	logredact.Install()
	if err := prepareStartupEnvironment(os.Args[1:], os.Stderr); err != nil {
		if errors.Is(err, flag.ErrHelp) {
			return
		}
		log.Fatalf("startup environment: %v", err)
	}
	shutdownCtx, stopShutdownSignals := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stopShutdownSignals()
	cfg := buildConfig()
	if err := runProxy(shutdownCtx, cfg); err != nil {
		log.Fatal(err)
	}
}

type proxyHandler struct {
	passportConfig      passportConfig
	ipAccess            ipAccessPolicy
	qwenCloudTokens     qwenCloudTokenCache
	qwenLogin           qwenLoginSessions
	antigravityOAuth    antigravityOAuthSessionStore
	antigravityLoopback antigravityLoopbackListeners
	antigravityReplay   antigravityReplayCache
	claudeOAuth         claudeOAuthSessions
	codexOAuth          codexOAuthSessions
	zaiLogin            zaiLoginSessions
	claudeIdentity      claudeIdentity
	verificationRuntime verificationRuntimeCache

	requestQueue         *fairQueue
	cfg                  *config
	transport            http.RoundTripper
	antigravityTransport http.RoundTripper
	refreshTransport     http.RoundTripper // Separate transport for refresh ops (may use proxy)
	pool                 *poolState
	passport             *PassportStore
	registry             *ProviderRegistry
	store                *usageStore
	nativeContext        *nativeContext
	contextHandoff       *conversationHandoffStore
	analyticsStore       costAnalyticsRepository
	duckAnalytics        *DuckAnalytics
	pricing              *PricingData
	aliases              *modelAliases
	effortCap            *effortCap
	bruteForce           *bruteForceTracker
	accountLocks         *bruteForceTracker
	passkeyIssueMu       sync.Mutex
	passkeyIssues        map[string]passkeyIssueWindow
	metrics              *metrics
	routeTraces          *routeTraceStore
	experiments          *experimentTracker
	trafficShadow        *trafficShadowRuntime
	poolAuto             *PoolAutoOrchestrator
	recent               *recentErrors
	inflight             int64
	startTime            time.Time
	pacer                *requestPacer // Per-session request pacing
	webSocketRegistryMu  sync.Mutex
	webSockets           *webSocketRegistry
	runnerBroker         *sessionBroker
	eventHeartbeat       time.Duration // SSE re-auth interval override for tests; zero uses 15s
	// Large JSON rewrites temporarily hold several copies of a request. This
	// deliberately serializes only those requests on a small production host.
	largeReplayBodies chan struct{}

	refreshCoordinator accountRefreshCoordinator

	usagePollMu    sync.Mutex
	quotaIntelMu   sync.RWMutex
	quotaIntelBusy bool
	quotaIntel     quotaIntelligenceSnapshot

	// turnStateOrigins tracks which Codex account minted the
	// x-codex-turn-state blob each downstream session currently holds, so
	// failover retries and hot-swaps can strip a known cross-account echo
	// instead of replaying it to an account that did not mint it.
	turnStateMu      sync.Mutex
	turnStateOrigins map[string]codexTurnStateOrigin
	turnStateWrites  uint64

	fanoutTokenMu sync.Mutex
	fanoutToken   string
}

const largeReplayBodyThreshold = 8 * 1024 * 1024

// acquireLargeReplayBody serializes requests whose JSON rewrite retains large
// raw, decoded, and re-marshaled copies simultaneously. Small requests remain
// fully concurrent. A missing channel keeps lightweight test handlers working.
func (h *proxyHandler) acquireLargeReplayBody(ctx context.Context, contentLength int64) (func(), error) {
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	if h == nil || h.largeReplayBodies == nil || contentLength >= 0 && contentLength <= largeReplayBodyThreshold {
		return func() {}, nil
	}
	select {
	case h.largeReplayBodies <- struct{}{}:
		return sync.OnceFunc(func() { <-h.largeReplayBodies }), nil
	case <-ctx.Done():
		return nil, ctx.Err()
	}
}

func (h *proxyHandler) pickUpstream(path string, headers http.Header) (ProviderDriver, *url.URL) {
	// Check headers first - Anthropic requests have X-Api-Key or anthropic-* headers
	if headers.Get("X-Api-Key") != "" {
		// X-Api-Key is used by Anthropic Claude API
		if provider := h.registry.ForType(AccountTypeClaude); provider != nil {
			return provider, provider.UpstreamURL(path)
		}
		return nil, nil
	}
	// Check for any anthropic-* headers (version, beta, etc.)
	for key := range headers {
		if strings.HasPrefix(strings.ToLower(key), "anthropic-") {
			if provider := h.registry.ForType(AccountTypeClaude); provider != nil {
				return provider, provider.UpstreamURL(path)
			}
			return nil, nil
		}
	}

	provider := h.registry.ForPath(path)
	if provider == nil {
		return nil, nil
	}
	return provider, provider.UpstreamURL(path)
}

func (h *proxyHandler) isOperatorOrAdmin(r *http.Request) bool {
	if h == nil || r == nil {
		return false
	}
	if h.cfg != nil && h.cfg.adminToken != "" && secureSecretEquals(r.Header.Get("X-Admin-Token"), h.cfg.adminToken) {
		return true
	}
	if h.passport != nil {
		if principal, _ := h.passport.authenticate(r); principal != nil && principal.Kind == PrincipalOperator {
			return true
		}
	}
	return false
}

func extractConversationIDFromHeaders(headers http.Header) string {
	for _, key := range []string{
		"session_id",
		"Session_id",
		"Session-Id",
		"conversation_id",
		"Conversation_id",
		"prompt_cache_key",
		"x-codex-conversation-id",
	} {
		if value := strings.TrimSpace(headers.Get(key)); value != "" {
			return value
		}
		for actualKey, values := range headers {
			if !strings.EqualFold(actualKey, key) {
				continue
			}
			for _, value := range values {
				if value = strings.TrimSpace(value); value != "" {
					return value
				}
			}
		}
	}
	return ""
}

func (h *proxyHandler) pinConversationToCyberAccess(conversationID string, accountType AccountType, requiredPlan, clientIP, currentAccountID, reqID string) bool {
	if conversationID == "" || accountType != AccountTypeCodex {
		return false
	}
	exclude := map[string]bool{}
	if currentAccountID != "" {
		exclude[currentAccountID] = true
	}
	acc := h.pool.selectRoute(routing.Request{
		Mode: routing.MatchCyberAccess, Exclude: exclude, Provider: string(accountType),
		RequiredPlan: requiredPlan, ClientIP: clientIP,
	}).Account
	if acc == nil {
		if h.cfg.debug.Load() {
			log.Printf("[%s] cyber_policy seen for conversation %s, but no cyber_access account is available", reqID, conversationID)
		}
		return false
	}
	h.pool.pin(conversationID, acc.ID)
	if h.cfg.debug.Load() {
		log.Printf("[%s] pinned conversation %s to cyber_access account %s after cyber_policy", reqID, conversationID, acc.ID)
	}
	return true
}

// shouldRetryBufferedSSEForCyberPolicy decides whether a buffered
// SSE-translation response that hit cyber_policy mid-stream should be
// discarded and retried against a cyber_access account on the next
// attempt of the request loop. Returns true only when (a) we already
// pinned the conversation to a cyber account during this attempt's SSE
// callback (meaning a candidate exists), (b) we actually saw a
// cyber_policy event (gated by acc not being CyberAccess), and (c) the
// caller has retries left. The buffered translator output is empty in
// this case anyway, so retrying is strictly better UX than writing the
// empty translation.
func (h *proxyHandler) shouldRetryBufferedSSEForCyberPolicy(cyberPinned bool, attempt, attempts int, acc *Account, reqID, label string) bool {
	if !cyberPinned || attempt >= attempts || acc == nil || acc.codex.CyberAccess {
		return false
	}
	log.Printf("[%s] buffered %s SSE saw cyber_policy on account %s; retrying on cyber_access account", reqID, label, acc.ID)
	if h.metrics != nil {
		h.metrics.incCyberPolicy(acc.ID, "retry_buffered")
	}
	return true
}

// wrapBufferedSSEWithCyberDetector returns a writer that forwards SSE
// bytes to the buffered translator under, but also drops cyber_policy
// events and flips *cyberPinned when one is seen. Used by the
// non-streaming buffered paths where we can't synthesize a refusal
// inline (the buffered translator owns the final shape) — the loop's
// retry mechanism then discards the buffer and tries a cyber account.
func (h *proxyHandler) wrapBufferedSSEWithCyberDetector(under io.Writer, accountType AccountType, acc *Account, conversationID, requiredPlan, originIP, reqID string, cyberPinned *bool) io.Writer {
	if accountType != AccountTypeCodex || acc == nil || acc.codex.CyberAccess {
		return under
	}
	return &sseInterceptWriter{
		w: under,
		onEvent: func(eventData []byte) (bool, bool) {
			if !isCyberPolicyError(eventData) {
				return false, false
			}
			if h.metrics != nil {
				h.metrics.incCyberPolicy(acc.ID, "suppressed_buffered")
			}
			if h.pinConversationToCyberAccess(conversationID, accountType, requiredPlan, originIP, acc.ID, reqID) {
				if cyberPinned != nil {
					*cyberPinned = true
				}
			}
			return true, true
		},
	}
}

func removeConflictingProxyHeaders(h http.Header) {
	// Remove ALL Cloudflare headers (Cf-*) — our own Cloudflare adds these,
	// and they confuse upstream Cloudflare (e.g. chatgpt.com) into blocking us.
	for key := range h {
		if strings.HasPrefix(strings.ToLower(key), "cf-") {
			h.Del(key)
		}
	}
	h.Del("Cdn-Loop")
	// Remove proxy/forwarding headers added by Caddy or Cloudflare
	h.Del("X-Forwarded-For")
	h.Del("X-Forwarded-Proto")
	h.Del("X-Forwarded-Host")
	h.Del("X-Real-Ip")
	h.Del("Via")
	h.Del("True-Client-Ip")
}

func normalizePath(basePath, incoming string) string {
	if basePath == "" || basePath == "/" {
		return incoming
	}
	if strings.HasPrefix(incoming, basePath) {
		trimmed := strings.TrimPrefix(incoming, basePath)
		if !strings.HasPrefix(trimmed, "/") {
			trimmed = "/" + trimmed
		}
		return trimmed
	}
	return incoming
}

func singleJoin(basePath, reqPath string) string {
	if basePath == "" || basePath == "/" {
		return reqPath
	}
	if strings.HasSuffix(basePath, "/") && strings.HasPrefix(reqPath, "/") {
		return basePath + strings.TrimPrefix(reqPath, "/")
	}
	if !strings.HasSuffix(basePath, "/") && !strings.HasPrefix(reqPath, "/") {
		return basePath + "/" + reqPath
	}
	return basePath + reqPath
}

// looksLikeProviderCredential checks if a token looks like a real provider credential
// that should be passed through directly rather than replaced with pool credentials.
func looksLikeProviderCredential(authHeader string) (bool, AccountType) {
	if !strings.HasPrefix(authHeader, "Bearer ") {
		return false, ""
	}
	token := strings.TrimPrefix(authHeader, "Bearer ")
	if token == "" {
		return false, ""
	}

	// Pool-generated Claude tokens should NOT be passed through. The retired
	// legacy prefix is matched literally so stale tokens never reach upstream.
	if strings.HasPrefix(token, ClaudePoolTokenPrefix) || strings.HasPrefix(token, "sk-ant-api-pool-") {
		return false, ""
	}

	// Claude/Anthropic API keys: sk-ant-api* or sk-ant-oat* (OAuth tokens)
	if strings.HasPrefix(token, "sk-ant-") {
		return true, AccountTypeClaude
	}

	// OpenAI-style API keys: sk-proj-*, sk-* (but not sk-ant-)
	if strings.HasPrefix(token, "sk-proj-") || (strings.HasPrefix(token, "sk-") && !strings.HasPrefix(token, "sk-ant-")) {
		return true, AccountTypeCodex
	}

	// Google OAuth tokens typically start with ya29. (access tokens)
	// But NOT pool tokens which are ya29.pool-*
	if strings.HasPrefix(token, "ya29.") && !strings.HasPrefix(token, "ya29.pool-") {
		return true, AccountTypeGemini
	}

	return false, ""
}

// isClaudePoolToken checks if the auth header contains a pool-generated Claude token.
// Returns (isPoolToken, userID) if valid.
func isClaudePoolToken(secret, authHeader string) (bool, string) {
	if secret == "" {
		return false, ""
	}
	if !strings.HasPrefix(authHeader, "Bearer ") {
		return false, ""
	}
	token := strings.TrimPrefix(authHeader, "Bearer ")
	userID, valid := parseClaudePoolToken(secret, token)
	return valid, userID
}

func applyStreamingResponseHeaders(header http.Header) {
	header.Set("X-Accel-Buffering", "no")
	header.Set("Connection", "keep-alive")
	cacheControl := header.Get("Cache-Control")
	if cacheControl == "" {
		header.Set("Cache-Control", "no-cache, no-transform")
	} else if !strings.Contains(strings.ToLower(cacheControl), "no-transform") {
		header.Set("Cache-Control", cacheControl+", no-transform")
	}
}

func shouldSampleResponseBodyForRequest(provider Codec, acc *Account, path string, resp *http.Response, translateDir wire.TranslateDirection, conversationID string, logBodies bool) bool {
	if provider == nil || resp == nil {
		return true
	}
	if logBodies || translateDir != wire.TranslateNone || conversationID != "" {
		return true
	}
	if !provider.DetectsSSE(path, resp.Header.Get("Content-Type")) {
		return true
	}
	if provider.WireFormat() == WireResponses {
		if acc != nil && !acc.codex.CyberAccess {
			return true
		}
		return conversationID == ""
	}
	return false
}

func (h *proxyHandler) updateUsageFromBody(a *Account, sample []byte, userID, originID string, admissions ...*policyAdmission) {
	if a == nil || len(sample) == 0 {
		return
	}
	lines := bytes.Split(sample, []byte("\n"))
	for _, line := range lines {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			line = bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		}
		if bytes.Equal(line, []byte("[DONE]")) {
			continue
		}
		var obj map[string]any
		if err := json.Unmarshal(line, &obj); err != nil {
			continue
		}

		// Handle Codex token_count events: {type: "token_count", info: {...}, rate_limits: {...}}
		if objType, _ := obj["type"].(string); objType == "token_count" {
			ru := parseTokenCountEvent(obj)
			if ru != nil {
				ru.AccountID = a.ID
				ru.UserID = userID
				ru.OriginID = originID
				ru.AccountType = a.Type
				a.mu.Lock()
				ru.PlanType = a.PlanType
				a.mu.Unlock()
				h.recordUsage(a, *ru, admissions...)
			}
			// Also apply rate limits from token_count
			if rl, ok := obj["rate_limits"].(map[string]any); ok {
				a.applyRateLimitsFromTokenCount(rl)
			}
			continue
		}

		// Legacy: rate_limit at top level
		if rl, ok := obj["rate_limit"].(map[string]any); ok {
			a.applyRateLimitObject(rl)
		}

		// Legacy: response object with usage
		if resp, ok := obj["response"].(map[string]any); ok {
			if rl, ok := resp["rate_limit"].(map[string]any); ok {
				a.applyRateLimitObject(rl)
			}
			if ru := parseRequestUsage(resp); ru != nil {
				ru.AccountID = a.ID
				ru.UserID = userID
				ru.OriginID = originID
				ru.AccountType = a.Type
				a.mu.Lock()
				ru.PlanType = a.PlanType
				a.mu.Unlock()
				h.recordUsage(a, *ru, admissions...)
			}
		}

		// Legacy: direct usage object
		if ru := parseRequestUsage(obj); ru != nil {
			ru.AccountID = a.ID
			ru.UserID = userID
			ru.OriginID = originID
			ru.AccountType = a.Type
			a.mu.Lock()
			ru.PlanType = a.PlanType
			a.mu.Unlock()
			h.recordUsage(a, *ru, admissions...)
		}
	}
}
