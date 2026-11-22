package main

import (
	"bytes"
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"syscall"
	"time"

	"codex-pool-proxy/internal/logredact"
	"codex-pool-proxy/internal/ratelimit"
	"codex-pool-proxy/internal/routing"
	"codex-pool-proxy/internal/wire"
	"github.com/klauspost/compress/zstd"
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

func bodyForInspection(r *http.Request, body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	enc := ""
	if r != nil {
		enc = strings.ToLower(r.Header.Get("Content-Encoding"))
	}
	looksGzip := len(body) >= 2 && body[0] == 0x1f && body[1] == 0x8b
	if strings.Contains(enc, "gzip") || looksGzip {
		gr, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return body
		}
		defer gr.Close()
		decoded, err := io.ReadAll(io.LimitReader(gr, 512*1024))
		if err != nil || len(decoded) == 0 {
			return body
		}
		return decoded
	}
	return body
}

// decodeRequestBody makes compressed JSON routable and rewritable. We must not
// forward encoded bytes after a model-based provider override: the selected
// provider needs the rewritten JSON, not the original Codex wire payload.
func decodeRequestBody(contentEncoding string, body []byte, maxBytes int64) ([]byte, bool, error) {
	encoding := strings.ToLower(strings.TrimSpace(contentEncoding))
	if len(body) == 0 || encoding == "" || encoding == "identity" {
		return body, false, nil
	}
	if maxBytes <= 0 {
		maxBytes = 16 << 20
	}

	var reader io.ReadCloser
	switch encoding {
	case "gzip":
		gzipReader, err := gzip.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, false, fmt.Errorf("decode gzip request body: %w", err)
		}
		reader = gzipReader
	case "zstd":
		zstdReader, err := zstd.NewReader(bytes.NewReader(body))
		if err != nil {
			return nil, false, fmt.Errorf("decode zstd request body: %w", err)
		}
		reader = zstdReader.IOReadCloser()
	default:
		return body, false, nil
	}
	defer reader.Close()

	decoded, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return nil, false, fmt.Errorf("decode %s request body: %w", encoding, err)
	}
	if int64(len(decoded)) > maxBytes {
		return nil, false, fmt.Errorf("decoded request body exceeds %d byte limit", maxBytes)
	}
	return decoded, true, nil
}

// clientOrDefaultTimeout picks the request timeout. If the client sent X-Stainless-Timeout
// (Anthropic SDK), use that. Otherwise fall back to streaming vs non-streaming defaults.
func clientOrDefaultTimeout(r *http.Request, reqTimeout, streamTimeout time.Duration, body []byte) time.Duration {
	var obj any
	if json.Unmarshal(body, &obj) != nil {
		obj = nil
	}
	request, _ := obj.(map[string]any)
	stream, _ := request["stream"].(bool)
	return timeoutForRequestIntent(r, reqTimeout, streamTimeout, requestIntent{
		stream: stream,
		image:  valueHasImageGenerationTool(obj),
	})
}

type requestIntent struct {
	stream bool
	image  bool
}

func timeoutForRequestIntent(r *http.Request, reqTimeout, streamTimeout time.Duration, intent requestIntent) time.Duration {
	const codexExpectedStreamTimeout = 5 * time.Minute

	isStreaming := intent.stream || strings.Contains(strings.ToLower(r.Header.Get("Accept")), "text/event-stream")
	isImageGeneration := intent.image

	// Streaming requests can run for a long time. Use the configured stream
	// timeout only; a zero value means no hard cap.
	if isStreaming {
		return streamTimeout
	}

	const maxClientRequestTimeout = 10 * time.Minute

	// Honour SDK/client-requested timeouts for non-streaming requests, but do
	// not let short 60s/120s client defaults cut off image generation before
	// Codex's expected 300s window. Bound against arbitrary client values to prevent
	// lingering connection denial-of-service.
	if v := r.Header.Get("X-Stainless-Timeout"); v != "" {
		if secs, err := strconv.ParseFloat(v, 64); err == nil && secs > 0 {
			requested := time.Duration(secs * float64(time.Second))
			if requested > maxClientRequestTimeout {
				requested = maxClientRequestTimeout
			}
			if isImageGeneration && requested < codexExpectedStreamTimeout {
				return codexExpectedStreamTimeout
			}
			return requested
		}
	}
	if isImageGeneration && reqTimeout > 0 && reqTimeout < codexExpectedStreamTimeout {
		return codexExpectedStreamTimeout
	}
	return reqTimeout
}

func (h *proxyHandler) logRateLimitResponseHeaders(reqID string, accountType AccountType, hdr http.Header) {
	if h == nil || !h.cfg.debug.Load() {
		return
	}
	if hdr == nil {
		return
	}

	keys := []string{
		"anthropic-ratelimit-unified-primary-utilization",
		"anthropic-ratelimit-unified-secondary-utilization",
		"anthropic-ratelimit-unified-tokens-utilization",
		"anthropic-ratelimit-unified-requests-utilization",
		"anthropic-ratelimit-unified-5h-utilization",
		"anthropic-ratelimit-unified-7d-utilization",
		"anthropic-ratelimit-unified-reset",
		"anthropic-ratelimit-unified-primary-reset",
		"anthropic-ratelimit-unified-secondary-reset",
		"anthropic-ratelimit-unified-tokens-reset",
		"anthropic-ratelimit-unified-requests-reset",
		"anthropic-ratelimit-unified-5h-reset",
		"anthropic-ratelimit-unified-7d-reset",
		"anthropic-ratelimit-unified-status",
		"anthropic-ratelimit-unified-5h-status",
		"anthropic-ratelimit-unified-7d-status",
		"x-ratelimit-limit-requests",
		"x-ratelimit-remaining-requests",
		"x-ratelimit-reset-requests",
		"x-ratelimit-limit-tokens",
		"x-ratelimit-remaining-tokens",
		"x-ratelimit-reset-tokens",
		"x-ratelimit-limit",
		"x-ratelimit-remaining",
		"x-ratelimit-reset",
	}

	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		if v := strings.TrimSpace(hdr.Get(key)); v != "" {
			parts = append(parts, key+"="+v)
		}
	}

	if len(parts) == 0 {
		for key := range hdr {
			lower := strings.ToLower(key)
			if strings.Contains(lower, "ratelimit") || strings.Contains(lower, "rate-limit") || strings.Contains(lower, "anthropic-ratelimit") {
				parts = append(parts, key+"="+hdr.Get(key))
			}
		}
	}

	if len(parts) == 0 {
		return
	}
	log.Printf("[%s] upstream %s rate-limit headers: %s", reqID, accountType, strings.Join(parts, ", "))
}

func isRateLimitError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "rate limited") || strings.Contains(msg, "too many requests") || strings.Contains(msg, "429")
}

// isPermanentRefreshTokenError reports auth-server failures that mean the
// refresh token can never recover without a fresh login. OpenAI currently
// returns code refresh_token_invalidated ("Your session has ended") rather
// than classic invalid_grant; both (and reuse) must retire the account.
func isPermanentRefreshTokenError(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	for _, needle := range []string{
		"invalid_grant",
		"refresh_token_reused",
		"refresh_token_invalidated",
		"session has ended",
	} {
		if strings.Contains(msg, needle) {
			return true
		}
	}
	return false
}

func parseRetryAfter(h http.Header) (time.Duration, bool) {
	if h == nil {
		return 0, false
	}
	val := strings.TrimSpace(h.Get("Retry-After"))
	if val == "" {
		return 0, false
	}
	if secs, err := strconv.ParseInt(val, 10, 64); err == nil {
		if secs <= 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if when, err := http.ParseTime(val); err == nil {
		wait := time.Until(when)
		if wait <= 0 {
			return 0, false
		}
		return wait, true
	}
	return 0, false
}

// backoffDuration returns the exponential backoff for the given level.
// Formula: min(1s * 2^level, 30m). Level 0 = 1s, 1 = 2s, 2 = 4s, ... 10 = ~17m, 11+ = 30m.
func backoffDuration(level int) time.Duration {
	const base = 1 * time.Second
	const maxBackoff = 30 * time.Minute
	d := base << uint(level) // 1s * 2^level
	if d > maxBackoff || d <= 0 {
		return maxBackoff
	}
	return d
}

func (h *proxyHandler) applyRateLimit(a *Account, hdr http.Header) time.Duration {
	if a == nil {
		return 0
	}

	// Try multiple sources for the cooldown duration, in priority order:
	// 1. Provider reset timestamp headers
	// 2. Retry-After header (standard HTTP)
	// 3. Exponential backoff (fallback)
	wait := time.Duration(0)
	gotPreciseReset := false

	if hdr != nil {
		// Prefer the latest explicit reset. A 429 can exhaust more than one
		// request or token window, and retrying at the earlier boundary just
		// produces another 429.
		var preciseReset time.Time
		for _, key := range []string{
			"anthropic-ratelimit-unified-reset",
			"anthropic-ratelimit-unified-primary-reset",
			"anthropic-ratelimit-unified-5h-reset",
			"x-ratelimit-reset-requests",
			"x-ratelimit-reset-tokens",
			"x-ratelimit-reset",
		} {
			if resetStr := hdr.Get(key); resetStr != "" {
				if resetAt, ok := ratelimit.ParseReset(resetStr); ok && resetAt.After(time.Now()) && resetAt.After(preciseReset) {
					preciseReset = resetAt
				}
			}
		}
		if !preciseReset.IsZero() {
			wait = time.Until(preciseReset)
			gotPreciseReset = true
		}
	}

	if !gotPreciseReset {
		if w, ok := parseRetryAfter(hdr); ok {
			wait = w
		} else {
			a.mu.Lock()
			wait = backoffDuration(a.BackoffLevel)
			a.mu.Unlock()
		}
	}

	// Always bump backoff level so repeated rate limits get longer fallbacks.
	a.mu.Lock()
	a.BackoffLevel++
	a.mu.Unlock()

	// Also parse full rate limit utilization from the 429 response headers.
	// This updates the account's usage snapshot so candidate selection
	// can factor in the current utilization level.
	if hdr != nil {
		snap, ok := parseClaudeResponseRateLimits(hdr)
		if ok {
			a.mu.Lock()
			a.Usage = mergeUsage(a.Usage, snap)
			a.mu.Unlock()
		}
	}

	until := time.Now().Add(wait)
	if wait <= 0 {
		return 0
	}

	a.mu.Lock()
	if a.RateLimitUntil.Before(until) {
		a.RateLimitUntil = until
	}
	a.mu.Unlock()
	if h.cfg.debug.Load() {
		log.Printf("rate-limit backoff: account=%s level=%d wait=%s precise=%v", a.ID, a.BackoffLevel, wait, gotPreciseReset)
	}
	return wait
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

func (h *proxyHandler) needsRefresh(a *Account) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.RefreshToken == "" || a.RefreshBlocked {
		return false
	}
	now := time.Now()

	// Per-account rate limiting: don't refresh too frequently
	// This prevents hammering the OAuth endpoint when refresh tokens are invalid
	if !a.LastRefresh.IsZero() && now.Sub(a.LastRefresh) < refreshPerAccountInterval {
		return false
	}

	// Antigravity refreshes with the same 3000-second headroom as the native
	// client so a long generation does not cross the token expiry boundary.
	if a.Type == AccountTypeAntigravity && !a.ExpiresAt.IsZero() && a.ExpiresAt.Before(now.Add(50*time.Minute)) {
		return true
	}

	// Codex access JWTs last about ten days. Refresh from the usage poller
	// when that window is nearly closed instead of spending the refresh token
	// on every WHAM/proxy 401.
	if a.Type == AccountTypeCodex && !a.ExpiresAt.IsZero() && a.ExpiresAt.Before(now.Add(codexRefreshHeadroom)) {
		return true
	}

	// Other providers refresh after expiry and recover early invalidation with
	// their existing same-account 401 retry.
	if !a.ExpiresAt.IsZero() && a.ExpiresAt.Before(now) {
		return true
	}
	// If no expiry time known, refresh after 12 hours since last refresh
	if a.ExpiresAt.IsZero() && !a.LastRefresh.IsZero() && now.Sub(a.LastRefresh) > 12*time.Hour {
		return true
	}
	return false
}

// codexRefreshHeadroom is how soon before access-token expiry the poller
// should rotate. Codex access JWTs carry an `exp` claim of about ten days.
const codexRefreshHeadroom = 6 * time.Hour

// refreshPerAccountInterval is the minimum time between refresh attempts for a single account
// This is persisted to disk and survives restarts, preventing hammering OAuth endpoints
// 15 minutes balances between preventing hammering and allowing recovery from expired tokens
const refreshPerAccountInterval = 15 * time.Minute

func (h *proxyHandler) refreshAccountAfterAuthFailure(ctx context.Context, a *Account) error {
	if a == nil {
		return errors.New("nil account")
	}
	if a.Type == AccountTypeCodex {
		// A 401 with a still-valid access JWT is not permission to spend the
		// refresh token. OpenAI treats reuse as "session has ended" and can
		// log out every copy of that login.
		if codexAccessFarFromExpiry(a, time.Now()) {
			return fmt.Errorf("codex access token still live, skipping refresh")
		}
		return h.refreshAccount(ctx, a)
	}
	a.mu.Lock()
	a.LastRefresh = time.Time{}
	a.RefreshBlocked = false
	a.mu.Unlock()
	return h.refreshAccount(ctx, a)
}

func (h *proxyHandler) refreshAccountOnce(ctx context.Context, a *Account) error {
	// Per-account rate limiting (persisted to disk via LastRefresh)
	a.mu.Lock()
	sinceLastRefresh := time.Since(a.LastRefresh)
	if !a.LastRefresh.IsZero() && sinceLastRefresh < refreshPerAccountInterval {
		a.mu.Unlock()
		return fmt.Errorf("account refresh rate limited (%s), wait %v", a.ID, refreshPerAccountInterval-sinceLastRefresh)
	}
	accType := a.Type
	a.mu.Unlock()

	if err := h.refreshCoordinator.waitForSlot(ctx); err != nil {
		return err
	}

	// Use the provider's RefreshToken method
	provider := h.registry.ForType(accType)
	if provider == nil {
		return fmt.Errorf("no provider for account type %s", accType)
	}
	err := provider.RefreshToken(ctx, a, h.refreshTransport)

	if err == nil {
		a.mu.Lock()
		a.LastRefresh = time.Now().UTC()
		if a.NeedsLogin || a.AuthError != "" {
			a.NeedsLogin = false
			a.AuthError = ""
		}
		a.mu.Unlock()
		if saveErr := saveAccount(h.pool.antigravity, a); saveErr != nil {
			log.Printf("warning: failed to save account %s after refresh: %v", a.ID, saveErr)
		}
	}

	return err
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
