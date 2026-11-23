package main

import (
	"flag"
	"fmt"
	"log"
	"math"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"codex-pool-proxy/internal/clientip"
	"codex-pool-proxy/internal/routing"
	"github.com/BurntSushi/toml"
)

// ConfigFile represents the config.toml structure.
type ConfigFile struct {
	ListenAddr      string  `toml:"listen_addr"`
	PoolDir         string  `toml:"pool_dir"`
	DBPath          string  `toml:"db_path"`
	MaxAttempts     int     `toml:"max_attempts"`
	DisableRefresh  bool    `toml:"disable_refresh"`
	RefreshProxyURL string  `toml:"refresh_proxy_url"` // HTTP proxy for refresh operations
	Debug           bool    `toml:"debug"`
	PublicURL       string  `toml:"public_url"`
	GrokBase        string  `toml:"grok_base"`
	AdminToken      string  `toml:"admin_token"`
	TierThreshold   float64 `toml:"tier_threshold"` // Secondary usage % threshold for tier preference (default 0.15)

	// ExhaustionWaitSeconds holds requests while all accounts of a type are
	// usage-exhausted, instead of failing fast with 503. 0 keeps the default.
	ExhaustionWaitSeconds int64 `toml:"exhaustion_wait_seconds"`

	ModelAliases map[string]string `toml:"model_aliases"`

	// Caps on Codex reasoning effort, keyed by pool user ID and by raw client
	// IP. Values are effort names ("medium", "high", ...).
	MaxReasoningEffortByUser   map[string]string `toml:"max_reasoning_effort_by_user"`
	MaxReasoningEffortByOrigin map[string]string `toml:"max_reasoning_effort_by_origin"`

	ClientPolicies map[string]ClientPolicy          `toml:"client_policies"`
	Providers      map[string]GenericProviderConfig `toml:"providers"`
	Experiments    ExperimentsConfig                `toml:"experiments"`
	Federation     FederationConfig                 `toml:"federation"`
	TrustedProxies []string                         `toml:"trusted_proxies"`

	// IP access policy: deny wins over allow; empty lists are unrestricted.
	IPAccessAllow []string `toml:"ip_access_allow"`
	IPAccessDeny  []string `toml:"ip_access_deny"`

	// IP privacy: stop persisting raw client IPs (wipes stored ones at
	// startup) and use salted hashes in traces. Defaults to enabled; set
	// ip_privacy = false (or PROXY_IP_PRIVACY=false) to keep raw IPs. The
	// hash salt rotates per window when origin_hash_window_hours > 0. Origin
	// metadata older than origin_retention_days is pruned (0 keeps
	// everything).
	IPPrivacy             *bool `toml:"ip_privacy"`
	OriginHashWindowHours int   `toml:"origin_hash_window_hours"`
	OriginRetentionDays   int   `toml:"origin_retention_days"`

	PoolUsers PoolUsersConfig   `toml:"pool_users"`
	Routing   RoutingConfigFile `toml:"routing"`
}

// PoolUsersConfig is the [pool_users] section.
type PoolUsersConfig struct {
	JWTSecret   string `toml:"jwt_secret"`
	StoragePath string `toml:"storage_path"`
}

type passportConfig struct {
	jwtSecret       string
	publicURL       string
	legacyUsersPath string
}

func resolvePassportConfig(file ConfigFile) passportConfig {
	usersPath := file.PoolUsers.StoragePath
	if usersPath == "" {
		usersPath = "./data/pool_users.json"
	}
	return passportConfig{
		jwtSecret:       getenv("POOL_JWT_SECRET", file.PoolUsers.JWTSecret),
		publicURL:       strings.TrimSuffix(getenv("PUBLIC_URL", file.PublicURL), "/"),
		legacyUsersPath: getenv("POOL_USERS_PATH", usersPath),
	}
}

// RoutingConfigFile is the [routing] section. Profiles can override any
// subset of the built-in weights under [routing.profiles.<name>].
type RoutingConfigFile struct {
	DefaultProfile string                     `toml:"default_profile"`
	DefaultModel   string                     `toml:"default_model"`
	Profiles       map[string]routing.Weights `toml:"profiles"`
}

// setHotReloadable publishes the fields the config watcher can refresh at
// runtime. Request goroutines read them through the hot* getters below.
func (c *config) setHotReloadable(tierThreshold float64, routing RoutingConfigFile, clientPolicies map[string]ClientPolicy, experiments ExperimentsConfig) {
	c.hotMu.Lock()
	c.tierThreshold = tierThreshold
	c.routing = routing
	c.clientPolicies = clientPolicies
	c.experiments = experiments
	c.hotMu.Unlock()
}

func (c *config) hotTierThreshold() float64 {
	c.hotMu.RLock()
	defer c.hotMu.RUnlock()
	return c.tierThreshold
}

func (c *config) hotRouting() RoutingConfigFile {
	c.hotMu.RLock()
	defer c.hotMu.RUnlock()
	return c.routing
}

func (c *config) hotClientPolicies() map[string]ClientPolicy {
	c.hotMu.RLock()
	defer c.hotMu.RUnlock()
	return c.clientPolicies
}

func (c *config) hotExperiments() ExperimentsConfig {
	c.hotMu.RLock()
	defer c.hotMu.RUnlock()
	return c.experiments
}

// loadConfigFile requires the selected file to exist and rejects unknown keys.
func loadConfigFile(path string) (*ConfigFile, error) {
	var cfg ConfigFile
	metadata, err := toml.DecodeFile(path, &cfg)
	if err != nil {
		return nil, err
	}
	if unknown := metadata.Undecoded(); len(unknown) > 0 {
		return nil, fmt.Errorf("unknown configuration key %s", unknown[0].String())
	}
	if err := validateConfigFile(&cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// Only an absent implicit config.toml selects env-only configuration. An
// explicitly selected path and all reloads require a readable valid file.
func loadStartupConfiguration() (string, *ConfigFile, error) {
	selected := os.Getenv("CONFIG_PATH")
	implicit := selected == ""
	if implicit {
		selected = "config.toml"
	}
	path, err := filepath.Abs(selected)
	if err != nil {
		return "", nil, err
	}
	cfg, err := loadConfigFile(path)
	if err != nil && os.IsNotExist(err) && implicit {
		return path, nil, nil
	}
	return path, cfg, err
}
func validateConfigFile(cfg *ConfigFile) error {
	if err := validateRoutingConfig(cfg.Routing); err != nil {
		return err
	}
	if err := validateTrafficShadowConfig(cfg.Experiments.Traffic); err != nil {
		return err
	}
	if cfg.MaxAttempts < 0 || cfg.MaxAttempts > math.MaxInt32 || cfg.ExhaustionWaitSeconds < 0 || cfg.ExhaustionWaitSeconds > math.MaxInt64/1000000000 || cfg.OriginHashWindowHours < 0 || cfg.OriginHashWindowHours > math.MaxInt64/3600000000000 || cfg.OriginRetentionDays < 0 || cfg.OriginRetentionDays > math.MaxInt64/86400000000000 {
		return fmt.Errorf("configuration limits are negative or overflow their supported range")
	}
	if math.IsNaN(cfg.TierThreshold) || math.IsInf(cfg.TierThreshold, 0) || cfg.TierThreshold < 0 || cfg.TierThreshold > 1 {
		return fmt.Errorf("tier_threshold must be between 0 and 1")
	}
	if _, err := clientip.ParseNetList("ip_access_allow", cfg.IPAccessAllow); err != nil {
		return err
	}
	if _, err := clientip.ParseNetList("ip_access_deny", cfg.IPAccessDeny); err != nil {
		return err
	}
	if _, _, err := clientip.ParseTrustedProxies(cfg.TrustedProxies); err != nil {
		return err
	}
	for model, rule := range cfg.Experiments.Canary {
		if strings.TrimSpace(model) == "" || strings.TrimSpace(rule.Candidate) == "" || math.IsNaN(rule.Percent) || math.IsInf(rule.Percent, 0) || rule.Percent < 0 || rule.Percent > 100 {
			return fmt.Errorf("invalid canary rule for %q", model)
		}
	}
	for key, p := range cfg.ClientPolicies {
		if strings.TrimSpace(key) == "" {
			return fmt.Errorf("empty client policy key")
		}
		l := p.Limits
		if err := validatePolicyLimits(l); err != nil {
			return fmt.Errorf("client policy %q: %w", key, err)
		}
		if p.Routing.Profile != "" {
			if err := validateRoutingConfig(RoutingConfigFile{DefaultProfile: p.Routing.Profile}); err != nil {
				return fmt.Errorf("client policy %q: %w", key, err)
			}
		}
		for _, list := range [][]string{p.Models.Allow, p.Models.Deny, p.Providers.Allow, p.Providers.Deny} {
			for _, v := range list {
				if strings.TrimSpace(v) == "" {
					return fmt.Errorf("client policy %q has an empty selector", key)
				}
			}
		}
	}
	for k, v := range cfg.ModelAliases {
		if strings.TrimSpace(k) == "" || strings.TrimSpace(v) == "" {
			return fmt.Errorf("invalid model alias %q", k)
		}
	}
	for _, caps := range []map[string]string{cfg.MaxReasoningEffortByUser, cfg.MaxReasoningEffortByOrigin} {
		for k, v := range caps {
			if _, ok := codexEffortRank[strings.ToLower(strings.TrimSpace(v))]; !ok || strings.TrimSpace(k) == "" {
				return fmt.Errorf("invalid reasoning effort cap for %q", k)
			}
		}
	}
	return nil
}
func configBoolValue(raw string) (bool, error) {
	switch strings.ToLower(strings.TrimSpace(raw)) {
	case "1", "true", "yes", "on", "enabled":
		return true, nil
	case "0", "false", "no", "off", "disabled":
		return false, nil
	default:
		return false, fmt.Errorf("invalid boolean")
	}
}
func validateConfigEnvironment() error {
	if _, ok := os.LookupEnv("TRUSTED_PROXIES"); ok {
		return fmt.Errorf("TRUSTED_PROXIES is unsupported; configure PROXY_TRUSTED_PROXIES")
	}
	// Explicit malformed settings never select defaults, including durations
	// that overflow time.Duration when converted from seconds/hours/days.
	specs := map[string]struct{ min, max int64 }{
		"PROXY_MAX_ATTEMPTS": {1, math.MaxInt32}, "PROXY_BODY_LOG_LIMIT": {1, math.MaxInt64}, "PROXY_CLAUDE_TRACE_BODY_LIMIT": {1, math.MaxInt64},
		"PROXY_MAX_INMEM_BODY_BYTES": {0, math.MaxInt64}, "PROXY_MAX_SPOOL_BODY_BYTES": {1, math.MaxInt64}, "PROXY_FLUSH_INTERVAL_MS": {0, math.MaxInt64 / 1000000},
		"PROXY_USAGE_REFRESH_SECONDS": {1, math.MaxInt64 / 1000000000}, "PROXY_EXHAUSTION_WAIT_SECONDS": {0, math.MaxInt64 / 1000000000},
		"PROXY_ORIGIN_HASH_WINDOW_HOURS": {0, math.MaxInt64 / 3600000000000}, "PROXY_ORIGIN_RETENTION_DAYS": {0, math.MaxInt64 / 86400000000000},
		"PROXY_USAGE_RETENTION_DAYS": {1, math.MaxInt32}, "PROXY_REQUEST_TIMEOUT_SECONDS": {0, math.MaxInt64 / 1000000000}, "PROXY_STREAM_TIMEOUT_SECONDS": {0, math.MaxInt64 / 1000000000},
		"STREAM_IDLE_TIMEOUT_SECONDS": {0, math.MaxInt64 / 1000000000}, "WEBSOCKET_IDLE_TIMEOUT_SECONDS": {0, math.MaxInt64 / 1000000000},
		"WEBSOCKET_HEARTBEAT_SECONDS": {0, math.MaxInt64 / 1000000000}, "WEBSOCKET_READ_LIMIT_BYTES": {1, math.MaxInt64}, "PROXY_SHUTDOWN_GRACE_SECONDS": {0, math.MaxInt64 / 1000000000},
		"CODEX_REQUEST_PACE_MS": {0, math.MaxInt64 / 1000000}, "ANALYTICS_EMERGENCY_RESERVE_BYTES": {1, math.MaxInt64},
	}
	for key, spec := range specs {
		if raw, ok := os.LookupEnv(key); ok {
			n, err := parseInt64(raw)
			if err != nil || n < spec.min || n > spec.max {
				return fmt.Errorf("%s must be an integer between %d and %d", key, spec.min, spec.max)
			}
		}
	}
	for _, key := range []string{"PROXY_DEBUG", "PROXY_DISABLE_REFRESH", "PROXY_LOG_BODIES", "PROXY_CLAUDE_TRACE_INCLUDE_SECRETS", "PROXY_IP_PRIVACY", "WEBSOCKET_COMPRESSION", "PROXY_TRUST_SAME_SUBNET"} {
		if raw, ok := os.LookupEnv(key); ok {
			if _, err := configBoolValue(raw); err != nil {
				return fmt.Errorf("%s: %w", key, err)
			}
		}
	}
	if raw, ok := os.LookupEnv("TIER_THRESHOLD"); ok {
		f, err := strconv.ParseFloat(raw, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) || f < 0 || f > 1 {
			return fmt.Errorf("TIER_THRESHOLD must be between 0 and 1")
		}
	}
	if _, _, err := clientip.ParseTrustedProxies(effectiveTrustedProxies(nil)); err != nil {
		return err
	}
	for _, key := range []string{"PROXY_IP_ALLOW", "PROXY_IP_DENY"} {
		if raw, ok := os.LookupEnv(key); ok {
			if _, err := clientip.ParseNetList(key, strings.Split(raw, ",")); err != nil {
				return err
			}
		}
	}
	return nil
}
func effectiveTrustedProxies(cfg []string) []string {
	if raw, ok := os.LookupEnv("PROXY_TRUSTED_PROXIES"); ok {
		if raw == "" {
			return nil
		}
		return strings.Split(raw, ",")
	}
	return cfg
}
func effectiveIPPolicy(cfg *ConfigFile) ([]string, []string) {
	allow, deny := cfg.IPAccessAllow, cfg.IPAccessDeny
	if raw, ok := os.LookupEnv("PROXY_IP_ALLOW"); ok {
		if raw == "" {
			allow = nil
		} else {
			allow = strings.Split(raw, ",")
		}
	}
	if raw, ok := os.LookupEnv("PROXY_IP_DENY"); ok {
		if raw == "" {
			deny = nil
		} else {
			deny = strings.Split(raw, ",")
		}
	}
	return allow, deny
}

// getConfigString returns the config value with priority: env var > config file > default.
func getConfigString(envKey string, configValue string, defaultValue string) string {
	if v, ok := os.LookupEnv(envKey); ok {
		return v
	}
	if configValue != "" {
		return configValue
	}
	return defaultValue
}

// getConfigInt returns the config value with priority: env var > config file > default.
func getConfigInt(envKey string, configValue int, defaultValue int) int {
	if v := os.Getenv(envKey); v != "" {
		n, err := parseInt64(v)
		if err != nil || n < 0 {
			log.Fatalf("invalid %s: expected a nonnegative integer", envKey)
		}
		return int(n)
	}
	if configValue < 0 {
		log.Fatalf("invalid %s configuration: negative value", envKey)
	}
	if configValue > 0 {
		return configValue
	}
	return defaultValue
}

// getConfigFloat64 returns the config value with priority: env var > config file > default.
func getConfigFloat64(envKey string, configValue float64, defaultValue float64) float64 {
	if v := os.Getenv(envKey); v != "" {
		f, err := strconv.ParseFloat(v, 64)
		if err != nil || math.IsNaN(f) || math.IsInf(f, 0) {
			log.Fatalf("invalid %s: expected a finite number", envKey)
		}
		return f
	}
	if configValue > 0 {
		return configValue
	}
	return defaultValue
}

// getConfigBool returns the config value with priority: env var > config file > default.
func getConfigBool(envKey string, configValue bool, defaultValue bool) bool {
	if v := os.Getenv(envKey); v != "" {
		b, err := configBoolValue(v)
		if err != nil {
			log.Fatalf("invalid %s: %v", envKey, err)
		}
		return b
	}
	if configValue {
		return true
	}
	return defaultValue
}

type config struct {
	vault                  credentialVault
	ipAccess               clientip.Settings
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
	cfg.ipAccess, err = clientip.ParseSettings(ipAllow, ipDeny, effectiveTrustedProxies(fileCfg.TrustedProxies), parseBoolEnv("PROXY_TRUST_SAME_SUBNET", false))
	if err != nil {
		log.Fatalf("invalid ip access policy: %v", err)
	}
	if cfg.ipAccess.Restricted() {
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
