package main

import (
	"fmt"
	"log"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"

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

// RoutingConfigFile is the [routing] section. Profiles can override any
// subset of the built-in weights under [routing.profiles.<name>].
type RoutingConfigFile struct {
	DefaultProfile string                           `toml:"default_profile"`
	DefaultModel   string                           `toml:"default_model"`
	Profiles       map[string]RoutingProfileWeights `toml:"profiles"`
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
	if _, err := parseIPNetList("ip_access_allow", cfg.IPAccessAllow); err != nil {
		return err
	}
	if _, err := parseIPNetList("ip_access_deny", cfg.IPAccessDeny); err != nil {
		return err
	}
	if _, _, err := parseTrustedProxies(cfg.TrustedProxies); err != nil {
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
		if l.RequestsPerMinute < 0 || l.ConcurrentRequests < 0 || l.DailyRequests < 0 || l.MonthlyRequests < 0 || l.DailyTokens < 0 || l.MonthlyTokens < 0 {
			return fmt.Errorf("client policy %q has negative limits", key)
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
	for _, key := range []string{"PROXY_DEBUG", "PROXY_DISABLE_REFRESH", "PROXY_LOG_BODIES", "PROXY_CLAUDE_TRACE_INCLUDE_SECRETS", "PROXY_EXHAUSTION_PREFER_WAIT", "PROXY_IP_PRIVACY", "WEBSOCKET_COMPRESSION", "PROXY_TRUST_SAME_SUBNET"} {
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
	if _, _, err := parseTrustedProxies(effectiveTrustedProxies(nil)); err != nil {
		return err
	}
	for _, key := range []string{"PROXY_IP_ALLOW", "PROXY_IP_DENY"} {
		if raw, ok := os.LookupEnv(key); ok {
			if _, err := parseIPNetList(key, strings.Split(raw, ",")); err != nil {
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
