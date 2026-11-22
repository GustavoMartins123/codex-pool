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
	"net"
	"net/http"
	"net/url"
	"os"
	"os/signal"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"time"

	"codex-pool-proxy/internal/logredact"
	"codex-pool-proxy/internal/ratelimit"
	"codex-pool-proxy/internal/routing"
	"codex-pool-proxy/internal/wire"
	"github.com/klauspost/compress/zstd"
)

type config struct {
	vault                  credentialVault
	ipAccess               ipAccessSettings
	passport               passportConfig
	configPath             string
	listenAddr             string
	responsesBase          *url.URL
	realtimeBase           *url.URL
	whamBase               *url.URL
	refreshBase            *url.URL
	geminiBase             *url.URL // Gemini CloudCode endpoint (for OAuth/Code Assist mode)
	geminiAPIBase          *url.URL // Gemini API endpoint (for API key mode)
	antigravityDailyBase   *url.URL
	antigravityProdBase    *url.URL
	antigravityOnboardBase *url.URL
	claudeBase             *url.URL // Claude API endpoint
	kimiBase               *url.URL // Kimi API endpoint
	minimaxBase            *url.URL // MiniMax API endpoint
	zaiBase                *url.URL // Z.ai Anthropic-compatible endpoint
	xiaomiBase             *url.URL // Xiaomi MiMo Token Plan Anthropic-compatible endpoint
	grokBase               *url.URL // Grok Code OpenAI-compatible endpoint
	adverserialBase        *url.URL // platform.adverserial.ai Anthropic-compatible endpoint
	opencodeGoBase         *url.URL // OpenCode Go subscription endpoint (zen/go/v1)
	qwenBase               *url.URL
	aistudioBase           *url.URL
	qwenPlanBase           *url.URL
	poolDir                string

	disableRefresh  bool
	refreshProxyURL string // HTTP proxy URL for refresh operations

	debug                      atomic.Bool
	logBodies                  bool
	bodyLogLimit               int64
	claudeTraceDir             string
	claudeTraceBodyLimit       int64
	claudeTraceSecrets         bool
	maxInMemoryBodyBytes       int64
	maxSpoolBodyBytes          int64
	flushInterval              time.Duration
	usageRefresh               time.Duration
	maxAttempts                int
	exhaustionWait             time.Duration // Hold requests while all accounts of a type are usage-exhausted (0 = fail fast)
	ipPrivacy                  bool          // Drop raw client IPs from persistence and traces (hash only)
	originHashWindow           time.Duration // Rotate the origin hash salt per window (0 = stable forever)
	originRetention            time.Duration // Retain origin metadata at most this long (0 = unlimited)
	storePath                  string
	retentionDays              int
	metricsToken               string
	adminToken                 string
	backupDir                  string
	restoreManifest            string
	decryptCredentials         bool
	checkCredentials           bool
	rotatePassportKey          bool
	duckPath                   string
	requestTimeout             time.Duration // Timeout for non-streaming requests (0 = no timeout)
	streamTimeout              time.Duration // Timeout for streaming/SSE requests (0 = no timeout)
	streamIdleTimeout          time.Duration // Kill SSE streams idle for this long (0 = no idle timeout)
	websocketIdleTimeout       time.Duration // Kill websocket relays idle for this long (0 = no idle timeout)
	websocketHeartbeatInterval time.Duration // Send downstream app-level websocket heartbeats this often (0 = disabled)
	websocketReadLimit         int64         // Maximum websocket message size
	websocketCompression       bool          // Enable per-message websocket compression (off by default for latency)
	shutdownGrace              time.Duration // Let active websocket turns finish before forcing a restart close
	tierThreshold              float64       // Secondary usage % at which we stop preferring a tier (default 0.50)
	routing                    RoutingConfigFile
	modelAliases               map[string]string
	maxReasoningEffortByUser   map[string]string
	maxReasoningEffortByOrigin map[string]string
	clientPolicies             map[string]ClientPolicy
	genericProviders           map[string]GenericProviderConfig
	experiments                ExperimentsConfig
	federation                 FederationConfig

	// hotMu guards the fields above that the config watcher republishes at
	// runtime while request goroutines read them.
	hotMu sync.RWMutex
}

func getenv(key, def string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return def
}

func mustParse(raw string) *url.URL {
	u, err := url.Parse(raw)
	if err != nil {
		log.Fatalf("invalid URL %q: %v", raw, err)
	}
	return u
}

func parseInt64(s string) (int64, error) { return strconv.ParseInt(s, 10, 64) }
func parseBoolEnv(key string, def bool) bool {
	raw, ok := os.LookupEnv(key)
	if !ok {
		return def
	}
	value, err := configBoolValue(raw)
	if err != nil {
		log.Fatalf("invalid %s: %v", key, err)
	}
	return value
}
func parseBoolEnvPtr(key string, cfg *bool, def bool) bool {
	if _, ok := os.LookupEnv(key); ok {
		return parseBoolEnv(key, def)
	}
	if cfg != nil {
		return *cfg
	}
	return def
}

// validateSecretStrength enforces fail-closed minimum lengths for the
// symmetric secrets that gate the deployment. A short POOL_JWT_SECRET lets
// attackers forge pool credentials; a short break-glass ADMIN_TOKEN or
// POOL_AUTH_ENCRYPTION_KEY weakens admin access and sealed token storage.
// Refuse to start instead of silently running with a guessable secret.
func validateSecretStrength(cfg *config) error {
	if secret := cfg.passport.jwtSecret; secret != "" && len(secret) < 32 {
		return fmt.Errorf("POOL_JWT_SECRET must be at least 32 characters (generate one with: openssl rand -hex 32)")
	}
	if key := os.Getenv("POOL_AUTH_ENCRYPTION_KEY"); key != "" && len(key) < 32 {
		return fmt.Errorf("POOL_AUTH_ENCRYPTION_KEY must be at least 32 characters (generate one with: openssl rand -hex 32)")
	}
	if cfg.metricsToken != "" && (len(cfg.metricsToken) < 32 || strings.ContainsAny(cfg.metricsToken, " \t\r\n")) {
		return fmt.Errorf("MONITORING_METRICS_TOKEN must be at least 32 characters without whitespace")
	}
	if cfg.adminToken != "" && len(cfg.adminToken) < 16 {
		return fmt.Errorf("ADMIN_TOKEN must be at least 16 characters (generate one with: openssl rand -hex 16)")
	}
	return nil
}

func configFilePath() string {
	if v := os.Getenv("CONFIG_PATH"); v != "" {
		return v
	}
	return "config.toml"
}

func buildConfig() *config {
	cfg := &config{}
	path, configFile, err := loadStartupConfiguration()
	cfg.configPath = path
	if err != nil {
		log.Fatalf("invalid config file %s: %v (refusing to start with defaults)", path, err)
	}
	if err := validateConfigEnvironment(); err != nil {
		log.Fatalf("invalid configuration environment: %v", err)
	}
	var fileCfg ConfigFile
	if configFile != nil {
		fileCfg = *configFile
	}

	cfg.passport = resolvePassportConfig(fileCfg)
	cfg.modelAliases = fileCfg.ModelAliases
	cfg.maxReasoningEffortByUser = fileCfg.MaxReasoningEffortByUser
	cfg.maxReasoningEffortByOrigin = fileCfg.MaxReasoningEffortByOrigin

	cfg.listenAddr = getConfigString("PROXY_LISTEN_ADDR", fileCfg.ListenAddr, "127.0.0.1:8989")
	cfg.responsesBase = mustParse(getenv("UPSTREAM_RESPONSES_BASE", "https://chatgpt.com/backend-api/codex"))
	// Realtime is served by the public OpenAI API, not the ChatGPT Codex
	// Responses backend. Codex OAuth access tokens are accepted there.
	cfg.realtimeBase = mustParse(getenv("UPSTREAM_REALTIME_BASE", "https://api.openai.com"))
	cfg.whamBase = mustParse(getenv("UPSTREAM_WHAM_BASE", "https://chatgpt.com/backend-api"))
	cfg.refreshBase = mustParse(getenv("UPSTREAM_REFRESH_BASE", "https://auth.openai.com"))
	cfg.geminiBase = mustParse(getenv("UPSTREAM_GEMINI_BASE", "https://cloudcode-pa.googleapis.com"))
	cfg.geminiAPIBase = mustParse(getenv("UPSTREAM_GEMINI_API_BASE", "https://generativelanguage.googleapis.com"))
	cfg.antigravityDailyBase = mustParse(getenv("UPSTREAM_ANTIGRAVITY_DAILY_BASE", "https://daily-cloudcode-pa.googleapis.com"))
	cfg.antigravityProdBase = mustParse(getenv("UPSTREAM_ANTIGRAVITY_BASE", "https://cloudcode-pa.googleapis.com"))
	cfg.antigravityOnboardBase = mustParse(getenv("UPSTREAM_ANTIGRAVITY_ONBOARD_BASE", "https://daily-cloudcode-pa.googleapis.com"))
	cfg.claudeBase = mustParse(getenv("UPSTREAM_CLAUDE_BASE", "https://api.anthropic.com"))
	cfg.kimiBase = mustParse(getenv("UPSTREAM_KIMI_BASE", "https://api.kimi.com/coding"))
	cfg.minimaxBase = mustParse(getenv("UPSTREAM_MINIMAX_BASE", "https://api.minimax.io/anthropic"))
	cfg.zaiBase = mustParse(getenv("UPSTREAM_ZAI_BASE", "https://api.z.ai/api/anthropic"))
	cfg.xiaomiBase = mustParse(getenv("UPSTREAM_XIAOMI_BASE", "https://token-plan-sgp.xiaomimimo.com/anthropic"))
	cfg.grokBase = mustParse(getConfigString("UPSTREAM_GROK_BASE", fileCfg.GrokBase, "https://cli-chat-proxy.grok.com/v1"))
	cfg.adverserialBase = mustParse(getenv("UPSTREAM_ADVERSERIAL_BASE", "https://platform.adverserial.ai/api"))
	cfg.opencodeGoBase = mustParse(getenv("UPSTREAM_OPENCODE_GO_BASE", "https://opencode.ai/zen/go/v1"))
	cfg.qwenBase = mustParse(getenv("UPSTREAM_QWEN_BASE", defaultQwenBase))
	cfg.aistudioBase = mustParse(getenv("UPSTREAM_AISTUDIO_BASE", defaultAIStudioBase))
	cfg.qwenPlanBase = mustParse(getenv("UPSTREAM_QWEN_PLAN_BASE", defaultQwenPlanBase))
	cfg.poolDir = getConfigString("POOL_DIR", fileCfg.PoolDir, "pool")

	// Refresh often fails for some auth.json fixtures; allow opting out.
	cfg.disableRefresh = getConfigBool("PROXY_DISABLE_REFRESH", fileCfg.DisableRefresh, false)
	cfg.refreshProxyURL = getConfigString("REFRESH_PROXY_URL", fileCfg.RefreshProxyURL, "")

	cfg.debug.Store(getConfigBool("PROXY_DEBUG", fileCfg.Debug, false))
	cfg.logBodies = parseBoolEnv("PROXY_LOG_BODIES", false)
	cfg.bodyLogLimit = 16 * 1024 // 16 KiB
	if v := getenv("PROXY_BODY_LOG_LIMIT", ""); v != "" {
		if n, err := parseInt64(v); err == nil && n > 0 {
			cfg.bodyLogLimit = n
		} else {
			log.Fatalf("invalid PROXY_BODY_LOG_LIMIT: malformed or out-of-range integer")
		}
	}
	cfg.claudeTraceDir = strings.TrimSpace(getenv("PROXY_CLAUDE_TRACE_DIR", ""))
	cfg.claudeTraceBodyLimit = 256 * 1024
	if v := getenv("PROXY_CLAUDE_TRACE_BODY_LIMIT", ""); v != "" {
		if n, err := parseInt64(v); err == nil && n > 0 {
			cfg.claudeTraceBodyLimit = n
		} else {
			log.Fatalf("invalid PROXY_CLAUDE_TRACE_BODY_LIMIT: malformed or out-of-range integer")
		}
	}
	cfg.claudeTraceSecrets = parseBoolEnv("PROXY_CLAUDE_TRACE_INCLUDE_SECRETS", false)
	cfg.maxInMemoryBodyBytes = 16 * 1024 * 1024 // 16 MiB
	if v := getenv("PROXY_MAX_INMEM_BODY_BYTES", ""); v != "" {
		if n, err := parseInt64(v); err == nil && n >= 0 {
			cfg.maxInMemoryBodyBytes = n
		} else {
			log.Fatalf("invalid PROXY_MAX_INMEM_BODY_BYTES: malformed or out-of-range integer")
		}
	}
	cfg.maxSpoolBodyBytes = 1024 * 1024 * 1024 // 1 GiB disk-backed request ceiling
	if v := getenv("PROXY_MAX_SPOOL_BODY_BYTES", ""); v != "" {
		if n, err := parseInt64(v); err == nil && n > 0 {
			cfg.maxSpoolBodyBytes = n
		} else {
			log.Fatalf("invalid PROXY_MAX_SPOOL_BODY_BYTES: malformed or out-of-range integer")
		}
	}
	cfg.flushInterval = 0
	if v := getenv("PROXY_FLUSH_INTERVAL_MS", ""); v != "" {
		if ms, err := parseInt64(v); err == nil && ms >= 0 {
			cfg.flushInterval = time.Duration(ms) * time.Millisecond
		} else {
			log.Fatalf("invalid PROXY_FLUSH_INTERVAL_MS: malformed or out-of-range integer")
		}
	}
	cfg.usageRefresh = 5 * time.Minute
	if v := getenv("PROXY_USAGE_REFRESH_SECONDS", ""); v != "" {
		if n, err := parseInt64(v); err == nil && n > 0 {
			cfg.usageRefresh = time.Duration(n) * time.Second
		} else {
			log.Fatalf("invalid PROXY_USAGE_REFRESH_SECONDS: malformed or out-of-range integer")
		}
	}
	cfg.maxAttempts = getConfigInt("PROXY_MAX_ATTEMPTS", fileCfg.MaxAttempts, 3)
	// Hold requests when every account of a type sits at the usage hard-exclude
	// thresholds, instead of failing fast with 503. 0 disables the hold.
	cfg.exhaustionWait = 15 * time.Minute
	if v := getenv("PROXY_EXHAUSTION_WAIT_SECONDS", ""); v != "" {
		if n, err := parseInt64(v); err == nil && n >= 0 {
			cfg.exhaustionWait = time.Duration(n) * time.Second
		} else {
			log.Fatalf("invalid PROXY_EXHAUSTION_WAIT_SECONDS: malformed or out-of-range integer")
		}
	} else if fileCfg.ExhaustionWaitSeconds > 0 {
		cfg.exhaustionWait = time.Duration(fileCfg.ExhaustionWaitSeconds) * time.Second
	}
	// IP privacy: stop persisting raw client IPs (and wipe previously stored
	// ones at startup). Origin analytics keep working on salted hashes.
	// Enabled unless the operator explicitly opts out, because the roadmap
	// requires raw IP persistence to be off by default.
	cfg.ipPrivacy = parseBoolEnvPtr("PROXY_IP_PRIVACY", fileCfg.IPPrivacy, true)
	cfg.originHashWindow = time.Duration(getConfigInt("PROXY_ORIGIN_HASH_WINDOW_HOURS", fileCfg.OriginHashWindowHours, 0)) * time.Hour
	cfg.originRetention = time.Duration(getConfigInt("PROXY_ORIGIN_RETENTION_DAYS", fileCfg.OriginRetentionDays, 0)) * 24 * time.Hour
	cfg.storePath = getConfigString("PROXY_DB_PATH", fileCfg.DBPath, "./data/proxy.db")
	cfg.adminToken = getConfigString("ADMIN_TOKEN", fileCfg.AdminToken, "")
	cfg.metricsToken = os.Getenv("MONITORING_METRICS_TOKEN")
	cfg.retentionDays = 30
	if v := getenv("PROXY_USAGE_RETENTION_DAYS", ""); v != "" {
		if n, err := parseInt64(v); err == nil && n > 0 {
			cfg.retentionDays = int(n)
		} else {
			log.Fatalf("invalid PROXY_USAGE_RETENTION_DAYS: malformed or out-of-range integer")
		}
	}
	ipAllow, ipDeny := effectiveIPPolicy(&fileCfg)
	cfg.ipAccess, err = parseIPAccessSettings(ipAllow, ipDeny, effectiveTrustedProxies(fileCfg.TrustedProxies), parseBoolEnv("PROXY_TRUST_SAME_SUBNET", false))
	if err != nil {
		log.Fatalf("invalid ip access policy: %v", err)
	}
	if cfg.ipAccess.restricted() {
		log.Printf("ip access policy active: allow=%v deny=%v", ipAllow, ipDeny)
	}

	// Request and stream timeouts default to disabled so long-running jobs can finish.
	// Set a positive value to enable a hard timeout.
	cfg.requestTimeout = 0
	if v := getenv("PROXY_REQUEST_TIMEOUT_SECONDS", ""); v != "" {
		if n, err := parseInt64(v); err == nil && n >= 0 {
			cfg.requestTimeout = time.Duration(n) * time.Second
		} else {
			log.Fatalf("invalid PROXY_REQUEST_TIMEOUT_SECONDS: malformed or out-of-range integer")
		}
	}
	cfg.streamTimeout = 0
	if v := getenv("PROXY_STREAM_TIMEOUT_SECONDS", ""); v != "" {
		if n, err := parseInt64(v); err == nil && n >= 0 {
			cfg.streamTimeout = time.Duration(n) * time.Second
		} else {
			log.Fatalf("invalid PROXY_STREAM_TIMEOUT_SECONDS: malformed or out-of-range integer")
		}
	}
	cfg.streamIdleTimeout = 0
	if v := getenv("STREAM_IDLE_TIMEOUT_SECONDS", ""); v != "" {
		if n, err := parseInt64(v); err == nil && n >= 0 {
			cfg.streamIdleTimeout = time.Duration(n) * time.Second
		} else {
			log.Fatalf("invalid STREAM_IDLE_TIMEOUT_SECONDS: malformed or out-of-range integer")
		}
	}
	cfg.websocketIdleTimeout = 0
	if v := getenv("WEBSOCKET_IDLE_TIMEOUT_SECONDS", ""); v != "" {
		if n, err := parseInt64(v); err == nil && n >= 0 {
			cfg.websocketIdleTimeout = time.Duration(n) * time.Second
		} else {
			log.Fatalf("invalid WEBSOCKET_IDLE_TIMEOUT_SECONDS: malformed or out-of-range integer")
		}
	}
	cfg.websocketHeartbeatInterval = heartbeatInterval
	if v := getenv("WEBSOCKET_HEARTBEAT_SECONDS", ""); v != "" {
		if n, err := parseInt64(v); err == nil && n >= 0 {
			cfg.websocketHeartbeatInterval = time.Duration(n) * time.Second
		} else {
			log.Fatalf("invalid WEBSOCKET_HEARTBEAT_SECONDS: malformed or out-of-range integer")
		}
	}
	cfg.websocketReadLimit = 64 * 1024 * 1024
	if v := getenv("WEBSOCKET_READ_LIMIT_BYTES", ""); v != "" {
		if n, err := parseInt64(v); err == nil && n > 0 {
			cfg.websocketReadLimit = n
		} else {
			log.Fatalf("invalid WEBSOCKET_READ_LIMIT_BYTES: malformed or out-of-range integer")
		}
	}
	cfg.websocketCompression = parseBoolEnv("WEBSOCKET_COMPRESSION", false)
	cfg.shutdownGrace = 30 * time.Second
	if v := getenv("PROXY_SHUTDOWN_GRACE_SECONDS", ""); v != "" {
		if n, err := parseInt64(v); err == nil && n >= 0 {
			cfg.shutdownGrace = time.Duration(n) * time.Second
		} else {
			log.Fatalf("invalid PROXY_SHUTDOWN_GRACE_SECONDS: malformed or out-of-range integer")
		}
	}

	// Tier threshold: secondary usage % at which we stop preferring a tier (default 50%)
	cfg.tierThreshold = getConfigFloat64("TIER_THRESHOLD", fileCfg.TierThreshold, 0.50)
	cfg.routing = fileCfg.Routing
	cfg.clientPolicies = fileCfg.ClientPolicies
	cfg.genericProviders = fileCfg.Providers
	cfg.experiments = fileCfg.Experiments
	cfg.federation = fileCfg.Federation
	if err := validateRoutingConfig(cfg.routing); err != nil {
		log.Fatalf("invalid routing config: %v", err)
	}

	registerStartupFlags(flag.CommandLine, cfg)
	flag.Parse()
	return cfg
}

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

func (h *proxyHandler) handleImagesGenerationFanout(w http.ResponseWriter, r *http.Request, body []byte, n int, reqID string) bool {
	if n <= 1 {
		return false
	}
	if n > 10 {
		http.Error(w, "n must be between 1 and 10", http.StatusBadRequest)
		return true
	}
	endpoint, err := h.imageFanoutEndpoint()
	if err != nil {
		http.Error(w, err.Error(), http.StatusServiceUnavailable)
		return true
	}
	oneBody := wire.SetImagesGenerationCount(body, 1)
	ctx := r.Context()
	client := &http.Client{Transport: h.transport, Timeout: clientOrDefaultTimeout(r, h.cfg.requestTimeout, h.cfg.streamTimeout, oneBody)}
	type imageFanoutResult struct {
		index   int
		body    []byte
		status  int
		header  http.Header
		err     error
		created int64
		data    []any
	}
	results := make([]imageFanoutResult, n)
	parallelism := h.pool.countByType(AccountTypeCodex)
	if parallelism < 1 {
		parallelism = 1
	}
	if parallelism > n {
		parallelism = n
	}
	sem := make(chan struct{}, parallelism)
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		i := i
		wg.Add(1)
		go func() {
			defer wg.Done()
			sem <- struct{}{}
			defer func() { <-sem }()
			const maxSlotAttempts = 3
			for slotAttempt := 1; slotAttempt <= maxSlotAttempts; slotAttempt++ {
				req, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(oneBody))
				if err != nil {
					results[i].err = err
					return
				}
				req.Header = cloneHeader(r.Header)
				removeHopByHopHeaders(req.Header)
				removeConflictingProxyHeaders(req.Header)
				req.Header.Del("Accept-Encoding")
				req.Header.Del("Content-Length")
				req.Header.Set("Content-Type", "application/json")
				req.Header.Set("X-Codex-Pool-Image-Fanout", "1")
				req.Header.Set("X-Codex-Pool-Image-Fanout-Index", strconv.Itoa(i))
				req.Header.Set("X-Codex-Pool-Image-Fanout-Attempt", strconv.Itoa(slotAttempt))
				req.Header.Set("X-Codex-Pool-Image-Fanout-Token", h.getFanoutToken())
				resp, err := client.Do(req)
				if err != nil {
					results[i].err = err
				} else {
					respBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 80*1024*1024))
					_ = resp.Body.Close()
					results[i].index = i
					results[i].status = resp.StatusCode
					results[i].header = resp.Header.Clone()
					results[i].body = respBody
					switch {
					case readErr != nil:
						results[i].err = readErr
					case resp.StatusCode < 200 || resp.StatusCode >= 300:
						results[i].err = fmt.Errorf("image fanout request %d failed: %s: %s", i+1, resp.Status, safeText(respBody))
					default:
						var data []any
						data, results[i].created, err = wire.AppendImagesGenerationData(nil, respBody, "b64_json")
						results[i].err = err
						results[i].data = data
					}
				}
				if results[i].err == nil {
					return
				}
				if results[i].status >= 400 && results[i].status < 500 {
					return
				}
				if slotAttempt < maxSlotAttempts {
					select {
					case <-ctx.Done():
						results[i].err = ctx.Err()
						return
					case <-time.After(time.Duration(slotAttempt) * 250 * time.Millisecond):
					}
				}
			}
		}()
	}
	wg.Wait()
	mergedData := []any{}
	created := time.Now().Unix()
	for _, result := range results {
		if result.err != nil {
			if h.cfg.debug.Load() {
				log.Printf("[%s] image fanout failed: %v", reqID, result.err)
			}
			http.Error(w, result.err.Error(), http.StatusBadGateway)
			return true
		}
		if result.created > 0 && (created == 0 || result.created < created) {
			created = result.created
		}
		mergedData = append(mergedData, result.data...)
	}
	out, err := json.Marshal(map[string]any{"created": created, "data": mergedData})
	if err != nil {
		http.Error(w, err.Error(), http.StatusInternalServerError)
		return true
	}
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(http.StatusOK)
	w.Write(out)
	return true
}

func (h *proxyHandler) imageFanoutEndpoint() (string, error) {
	if h == nil || h.cfg == nil {
		return "", fmt.Errorf("image fanout listen address unavailable")
	}
	_, port, err := net.SplitHostPort(h.cfg.listenAddr)
	if err != nil {
		return "", fmt.Errorf("invalid image fanout listen address")
	}
	number, err := strconv.Atoi(port)
	if err != nil || number < 1 || number > 65535 {
		return "", fmt.Errorf("invalid image fanout listen port")
	}
	return "http://127.0.0.1:" + port + "/v1/images/generations", nil
}

func (h *proxyHandler) getFanoutToken() string {
	if h == nil {
		return ""
	}
	h.fanoutTokenMu.Lock()
	defer h.fanoutTokenMu.Unlock()
	if h.fanoutToken == "" {
		h.fanoutToken = randomHex(16)
	}
	return h.fanoutToken
}

func (h *proxyHandler) isValidImageFanout(r *http.Request) bool {
	if h == nil || r == nil {
		return false
	}
	if r.Header.Get("X-Codex-Pool-Image-Fanout") == "" {
		return false
	}
	token := r.Header.Get("X-Codex-Pool-Image-Fanout-Token")
	return token != "" && token == h.getFanoutToken()
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

func mapResponsesPath(in string) string {
	// Only public/v1 Responses paths map to ChatGPT's /responses join form.
	// /backend-api/codex/responses must keep the /codex/responses suffix so
	// CodexProvider.NormalizePath + whamBase still produce
	// https://chatgpt.com/backend-api/codex/responses (not /backend-api/responses).
	switch {
	case strings.HasPrefix(in, "/v1/responses/compact") || strings.HasPrefix(in, "/responses/compact"):
		return "/responses/compact"
	case in == "/v1/responses" || in == "/responses":
		return "/responses"
	default:
		return ""
	}
}

// isCodexResponsesPath reports Codex-shaped Responses endpoints, including the
// private ChatGPT backend path clients often configure as OPENAI_BASE_URL.
// Oversized/chunked requests on these paths must still model-route: without
// that, Grok/Kimi/etc. are forwarded to ChatGPT and fail with
// "not supported when using Codex with a ChatGPT account".
func isCodexResponsesPath(path string) bool {
	path = normalizeNoopPath(path)
	if path == "/responses" || path == "/v1/responses" ||
		strings.HasPrefix(path, "/responses/") || strings.HasPrefix(path, "/v1/responses/") {
		return true
	}
	if path == "/backend-api/codex/responses" || strings.HasPrefix(path, "/backend-api/codex/responses/") {
		return true
	}
	if path == "/api/codex/responses" || strings.HasPrefix(path, "/api/codex/responses/") {
		return true
	}
	return false
}

func isStreamedModelRoutePath(path string) bool {
	path = normalizeNoopPath(path)
	return path == "/v1/messages" || isCodexResponsesPath(path)
}

func codexPassthroughNeedsBodyRewrite(path string) bool {
	return strings.HasPrefix(path, "/v1/messages") ||
		strings.HasPrefix(path, "/v1/chat/completions") ||
		strings.HasPrefix(path, "/v1/completions")
}

func ensureCodexResponsesInstructions(body []byte) []byte {
	if len(body) == 0 {
		return body
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body
	}
	prepareResponsesObject(obj)
	out, err := json.Marshal(obj)
	if err != nil {
		return body
	}
	return out
}

func prepareResponsesObject(obj map[string]any) {
	if _, ok := obj["instructions"]; !ok {
		obj["instructions"] = ""
	}
	obj["store"] = false
	obj["stream"] = true
	prepareCodexSchemasInBody(obj)
	wire.SanitizeCodexResponsesParams(obj)
	if input, ok := obj["input"].(string); ok {
		obj["input"] = []any{
			map[string]any{
				"type": "message",
				"role": "user",
				"content": []any{
					map[string]any{"type": "input_text", "text": input},
				},
			},
		}
	}
	stripHostedMCPFromResponsesRequest(obj)
}

func prepareCodexSchemasInBody(obj map[string]any) {
	if text, _ := obj["text"].(map[string]any); text != nil {
		if format, _ := text["format"].(map[string]any); format != nil {
			if schema, _ := format["schema"].(map[string]any); schema != nil {
				format["schema"] = wire.PrepareCodexJSONSchema(schema)
			}
		}
	}
	if tools, ok := obj["tools"].([]any); ok {
		for _, raw := range tools {
			tool, _ := raw.(map[string]any)
			if tool == nil {
				continue
			}
			if params, _ := tool["parameters"].(map[string]any); params != nil {
				tool["parameters"] = wire.PrepareCodexJSONSchema(params)
			}
			if fn, _ := tool["function"].(map[string]any); fn != nil {
				if params, _ := fn["parameters"].(map[string]any); params != nil {
					fn["parameters"] = wire.PrepareCodexJSONSchema(params)
				}
			}
		}
	}
}

func requestHasImageGenerationTool(body []byte) bool {
	var obj any
	if err := json.Unmarshal(body, &obj); err != nil {
		return false
	}
	return valueHasImageGenerationTool(obj)
}

func valueHasImageGenerationTool(v any) bool {
	switch x := v.(type) {
	case map[string]any:
		if typ, _ := x["type"].(string); typ == "image_generation" {
			return true
		}
		for _, child := range x {
			if valueHasImageGenerationTool(child) {
				return true
			}
		}
	case []any:
		for _, child := range x {
			if valueHasImageGenerationTool(child) {
				return true
			}
		}
	}
	return false
}

func codexPassthroughRewrite(path string, body []byte) (rewrittenPath string, rewrittenBody []byte, err error) {
	switch {
	case strings.HasPrefix(path, "/v1/messages"):
		rewritten, err := wire.AnthropicRequestToResponses(body)
		if err != nil {
			return path, nil, err
		}
		return "/v1/responses", rewritten, nil
	case strings.HasPrefix(path, "/v1/chat/completions"):
		rewritten, err := wire.OpenAIChatRequestToResponses(body)
		if err != nil {
			return path, nil, err
		}
		return "/v1/responses", rewritten, nil
	case strings.HasPrefix(path, "/v1/completions"):
		rewritten, err := wire.OpenAICompletionsRequestToResponses(body)
		if err != nil {
			return path, nil, err
		}
		return "/v1/responses", rewritten, nil
	default:
		return path, body, nil
	}
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

func extractConversationIDFromJSON(blob []byte) string {
	if len(blob) == 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(blob, &obj); err != nil {
		return ""
	}
	return extractConversationIDFromObject(obj)
}

func extractConversationIDFromObject(obj map[string]any) string {
	for _, key := range []string{"conversation_id", "conversation", "session_id", "prompt_cache_key"} {
		if v, ok := obj[key].(string); ok && v != "" {
			return v
		}
	}
	for _, containerKey := range []string{"client_metadata", "metadata", "meta"} {
		if sub, ok := obj[containerKey].(map[string]any); ok {
			for _, key := range []string{"conversation_id", "conversation", "session_id", "user_id"} {
				if v, ok := sub[key].(string); ok && v != "" {
					return v
				}
			}
		}
	}
	return ""
}

func extractRequestedModelFromJSON(blob []byte) string {
	if len(blob) == 0 {
		return ""
	}
	var obj map[string]any
	if err := json.Unmarshal(blob, &obj); err != nil {
		return ""
	}
	if v, ok := obj["model"].(string); ok {
		return strings.TrimSpace(v)
	}
	return ""
}

func stripCodexModelSuffixes(model string) (base string, reasoningEffort string, serviceTier string) {
	base = strings.TrimSpace(model)
	lower := strings.ToLower(base)
	for _, tier := range []string{"fast", "flex"} {
		suffix := "-" + tier
		if strings.HasSuffix(lower, suffix) {
			serviceTier = tier
			if serviceTier == "fast" {
				serviceTier = "priority"
			}
			base = strings.TrimSpace(base[:len(base)-len(suffix)])
			lower = strings.ToLower(base)
			break
		}
	}
	// Order matters: longer suffixes first so -xhigh wins over -high, etc.
	// GPT-5.6 adds max (above xhigh). ultra is multi-agent product mode;
	// map suffix to max for single-request routing through the pool.
	for _, effort := range []string{"xhigh", "minimal", "medium", "ultra", "none", "high", "max", "low"} {
		suffix := "-" + effort
		if strings.HasSuffix(lower, suffix) {
			reasoningEffort = effort
			if effort == "ultra" {
				reasoningEffort = "max"
			}
			base = strings.TrimSpace(base[:len(base)-len(suffix)])
			break
		}
	}
	return base, reasoningEffort, serviceTier
}

func applyCodexModelSuffixControls(body []byte, originalModel string) ([]byte, string) {
	base, effort, serviceTier := stripCodexModelSuffixes(originalModel)
	if base == "" || (base == originalModel && effort == "" && serviceTier == "") {
		return body, originalModel
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return body, originalModel
	}
	obj["model"] = base
	if effort != "" {
		reasoning, _ := obj["reasoning"].(map[string]any)
		if reasoning == nil {
			reasoning = map[string]any{}
		}
		if _, ok := reasoning["summary"]; !ok {
			reasoning["summary"] = "auto"
		}
		reasoning["effort"] = effort
		obj["reasoning"] = reasoning
	}
	if serviceTier != "" {
		obj["service_tier"] = serviceTier
	}
	rewritten, err := json.Marshal(obj)
	if err != nil {
		return body, originalModel
	}
	return rewritten, base
}

func modelRequiresCodexPro(model string) bool {
	return strings.EqualFold(strings.TrimSpace(model), "gpt-5.3-codex-spark")
}

const requiredPlanClaudePremium = "claude_premium"

func claudeRequestRequiresPremium(r *http.Request, model string) bool {
	model = strings.ToLower(strings.TrimSpace(model))
	// Opus is available on Pro. Only explicit extended-context requests use
	// this plan filter; regular model access is decided by Claude upstream.
	if strings.Contains(model, "[1m]") {
		return true
	}
	if r == nil {
		return false
	}
	for _, beta := range r.Header.Values("anthropic-beta") {
		if strings.Contains(strings.ToLower(beta), "context-1m-") {
			return true
		}
	}
	return false
}

func requiredPlanForRequest(accountType AccountType, r *http.Request, requestedModel string) string {
	if accountType == AccountTypeCodex && modelRequiresCodexPro(requestedModel) {
		return "pro"
	}
	if accountType == AccountTypeClaude && claudeRequestRequiresPremium(r, requestedModel) {
		return requiredPlanClaudePremium
	}
	return ""
}

func (h *proxyHandler) modelRouteOverride(path, model string, body []byte) (ProviderDriver, *url.URL, []byte) {
	provider, rewritten := h.registry.resolveBufferedModel(h.pool, model, body)
	if provider == nil {
		return nil, nil, nil
	}
	return provider, provider.UpstreamURL(path), rewritten
}

const streamedModelRoutePeekBytes = 64 * 1024

func shouldPeekStreamedModelRoute(r *http.Request) bool {
	if r == nil || r.Method != http.MethodPost || !isStreamedModelRoutePath(r.URL.Path) {
		return false
	}
	// Chunked Responses bodies never advertise a size. Peek anyway so a leading
	// "model" field can divert Grok/Kimi/etc. before Codex streaming fallthrough.
	if r.ContentLength < 0 {
		return isCodexResponsesPath(r.URL.Path)
	}
	return r.ContentLength >= streamedModelRoutePeekBytes
}

func (h *proxyHandler) applyStreamedModelRoute(r *http.Request, provider ProviderDriver, targetBase *url.URL, reqID string) (ProviderDriver, *url.URL, error) {
	if r == nil || r.Body == nil || h == nil || h.registry == nil {
		return provider, targetBase, nil
	}
	if r.Method != http.MethodPost || !isStreamedModelRoutePath(r.URL.Path) {
		return provider, targetBase, nil
	}
	if strings.TrimSpace(r.Header.Get("Content-Encoding")) != "" {
		return provider, targetBase, nil
	}
	contentType := strings.ToLower(r.Header.Get("Content-Type"))
	if contentType != "" && !strings.Contains(contentType, "json") {
		return provider, targetBase, nil
	}

	prefix, err := readBodyPrefix(r.Body, streamedModelRoutePeekBytes)
	if err != nil {
		r.Body = &prefixReadCloser{r: bytes.NewReader(prefix), c: r.Body}
		return provider, targetBase, err
	}
	restoreBody := func(replacement []byte) {
		r.Body = &prefixReadCloser{r: io.MultiReader(bytes.NewReader(replacement), r.Body), c: r.Body}
	}

	model, valueStart, valueEnd, ok := findTopLevelJSONStringField(prefix, "model")
	if !ok {
		restoreBody(prefix)
		return provider, targetBase, nil
	}

	requestedModel := strings.TrimSpace(model)
	if resolved, aliased := h.aliases.resolve(requestedModel); aliased {
		if h.cfg != nil && h.cfg.debug.Load() {
			log.Printf("[%s] streamed model alias: %s -> %s", reqID, requestedModel, resolved)
		}
		requestedModel = resolved
	}
	routeProvider, routeBase, canonicalModel := h.resolveStreamedModelRoute(r.URL.Path, requestedModel)
	if routeProvider == nil {
		restoreBody(prefix)
		return provider, targetBase, nil
	}
	if admission := policyAdmissionFromRequest(r); admission != nil {
		if err := admission.CheckModel(canonicalModel); err != nil {
			restoreBody(prefix)
			return provider, targetBase, err
		}
		if err := admission.CheckProvider(routeProvider.Type()); err != nil {
			restoreBody(prefix)
			return provider, targetBase, err
		}
	}
	rewrittenPrefix := prefix
	delta := 0
	if canonicalModel != requestedModel {
		rewrittenPrefix, delta, err = replaceJSONStringToken(prefix, valueStart, valueEnd, canonicalModel)
	}
	if err != nil {
		restoreBody(prefix)
		return provider, targetBase, err
	}
	if r.ContentLength >= 0 && delta != 0 {
		r.ContentLength += int64(delta)
		r.Header.Del("Content-Length")
	}
	restoreBody(rewrittenPrefix)
	return routeProvider, routeBase, nil
}

func (h *proxyHandler) resolveStreamedModelRoute(path, model string) (ProviderDriver, *url.URL, string) {
	provider, canonical := h.registry.resolveStreamedModel(h.pool, model)
	if provider == nil {
		return nil, nil, canonical
	}
	return provider, provider.UpstreamURL(path), canonical
}

func readBodyPrefix(body io.Reader, limit int64) ([]byte, error) {
	var buf bytes.Buffer
	_, err := io.CopyN(&buf, body, limit)
	if err == io.EOF {
		err = nil
	}
	return buf.Bytes(), err
}

type prefixReadCloser struct {
	r io.Reader
	c io.Closer
}

func (p *prefixReadCloser) Read(b []byte) (int, error) {
	return p.r.Read(b)
}

func (p *prefixReadCloser) Close() error {
	return p.c.Close()
}

func replaceJSONStringToken(prefix []byte, start, end int, value string) ([]byte, int, error) {
	if start < 0 || end < start || end > len(prefix) {
		return nil, 0, fmt.Errorf("invalid JSON string bounds")
	}
	encoded, err := json.Marshal(value)
	if err != nil {
		return nil, 0, err
	}
	out := make([]byte, 0, len(prefix)-(end-start)+len(encoded))
	out = append(out, prefix[:start]...)
	out = append(out, encoded...)
	out = append(out, prefix[end:]...)
	return out, len(encoded) - (end - start), nil
}

func findTopLevelJSONStringField(blob []byte, field string) (value string, valueStart int, valueEnd int, ok bool) {
	i := skipJSONWhitespace(blob, 0)
	if i >= len(blob) || blob[i] != '{' {
		return "", 0, 0, false
	}
	i++
	for {
		i = skipJSONWhitespace(blob, i)
		if i >= len(blob) {
			return "", 0, 0, false
		}
		if blob[i] == '}' {
			return "", 0, 0, false
		}
		if blob[i] == ',' {
			i++
			continue
		}
		key, _, _, next, ok := readJSONStringToken(blob, i)
		if !ok {
			return "", 0, 0, false
		}
		i = skipJSONWhitespace(blob, next)
		if i >= len(blob) || blob[i] != ':' {
			return "", 0, 0, false
		}
		i = skipJSONWhitespace(blob, i+1)
		if key == field {
			value, valueStart, valueEnd, _, ok = readJSONStringToken(blob, i)
			return value, valueStart, valueEnd, ok
		}
		next, ok = skipJSONValue(blob, i)
		if !ok {
			return "", 0, 0, false
		}
		i = next
	}
}

func skipJSONWhitespace(blob []byte, i int) int {
	for i < len(blob) {
		switch blob[i] {
		case ' ', '\n', '\r', '\t':
			i++
		default:
			return i
		}
	}
	return i
}

func readJSONStringToken(blob []byte, i int) (value string, start int, end int, next int, ok bool) {
	if i >= len(blob) || blob[i] != '"' {
		return "", 0, 0, 0, false
	}
	start = i
	escaped := false
	for i++; i < len(blob); i++ {
		if escaped {
			escaped = false
			continue
		}
		switch blob[i] {
		case '\\':
			escaped = true
		case '"':
			end = i + 1
			if err := json.Unmarshal(blob[start:end], &value); err != nil {
				return "", 0, 0, 0, false
			}
			return value, start, end, end, true
		}
	}
	return "", 0, 0, 0, false
}

func skipJSONValue(blob []byte, i int) (int, bool) {
	i = skipJSONWhitespace(blob, i)
	if i >= len(blob) {
		return 0, false
	}
	if blob[i] == '"' {
		_, _, _, next, ok := readJSONStringToken(blob, i)
		return next, ok
	}
	if blob[i] != '{' && blob[i] != '[' {
		for i < len(blob) {
			switch blob[i] {
			case ',', '}', ']':
				return i, true
			case ' ', '\n', '\r', '\t':
				return skipJSONWhitespace(blob, i), true
			default:
				i++
			}
		}
		return 0, false
	}

	stack := []byte{blob[i]}
	escaped := false
	inString := false
	for i++; i < len(blob); i++ {
		c := blob[i]
		if inString {
			if escaped {
				escaped = false
				continue
			}
			switch c {
			case '\\':
				escaped = true
			case '"':
				inString = false
			}
			continue
		}
		switch c {
		case '"':
			inString = true
		case '{', '[':
			stack = append(stack, c)
		case '}', ']':
			if len(stack) == 0 {
				return 0, false
			}
			open := stack[len(stack)-1]
			if (open == '{' && c != '}') || (open == '[' && c != ']') {
				return 0, false
			}
			stack = stack[:len(stack)-1]
			if len(stack) == 0 {
				return i + 1, true
			}
		}
	}
	return 0, false
}

// rewriteModelInBody replaces the "model" field in a JSON request body.
func rewriteModelInBody(body []byte, newModel string) []byte {
	if len(body) == 0 {
		return nil
	}
	var obj map[string]any
	if err := json.Unmarshal(body, &obj); err != nil {
		return nil
	}
	if _, ok := obj["model"]; !ok {
		return nil
	}
	obj["model"] = newModel
	rewritten, err := json.Marshal(obj)
	if err != nil {
		return nil
	}
	return rewritten
}

func extractConversationIDFromSSE(sample []byte) string {
	// Best-effort: scan lines for JSON fragments and grab conversation_id/conversation.
	for _, line := range bytes.Split(sample, []byte("\n")) {
		line = bytes.TrimSpace(line)
		if len(line) == 0 {
			continue
		}
		if bytes.HasPrefix(line, []byte("data:")) {
			line = bytes.TrimSpace(bytes.TrimPrefix(line, []byte("data:")))
		}
		if len(line) == 0 || bytes.Equal(line, []byte("[DONE]")) {
			continue
		}
		if id := extractConversationIDFromJSON(line); id != "" {
			return id
		}
	}
	return ""
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
