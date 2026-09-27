package main

import (
	"os"
	"strconv"

	"github.com/BurntSushi/toml"
)

// ConfigFile represents the config.toml structure.
type ConfigFile struct {
	ListenAddr       string  `toml:"listen_addr"`
	PoolDir          string  `toml:"pool_dir"`
	DBPath           string  `toml:"db_path"`
	MaxAttempts      int     `toml:"max_attempts"`
	DisableRefresh   bool    `toml:"disable_refresh"`
	RefreshProxyURL  string  `toml:"refresh_proxy_url"` // HTTP proxy for refresh operations
	Debug            bool    `toml:"debug"`
	PublicURL        string  `toml:"public_url"`
	GrokBase         string  `toml:"grok_base"`
	LegacyFriendCode string  `toml:"friend_code"` // transitional account-claim code and first-boot analytics salt seed
	AdminToken       string  `toml:"admin_token"`
	TierThreshold    float64 `toml:"tier_threshold"` // Secondary usage % threshold for tier preference (default 0.15)

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

	// IP access policy: deny always wins over allow, loopback is always
	// permitted, and empty lists leave the pool unrestricted.
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

// loadConfigFile loads config.toml if it exists.
// Returns nil if the file doesn't exist.
func loadConfigFile(path string) (*ConfigFile, error) {
	if _, err := os.Stat(path); os.IsNotExist(err) {
		return nil, nil
	}

	var cfg ConfigFile
	if _, err := toml.DecodeFile(path, &cfg); err != nil {
		return nil, err
	}
	return &cfg, nil
}

// getConfigString returns the config value with priority: env var > config file > default.
func getConfigString(envKey string, configValue string, defaultValue string) string {
	if v := os.Getenv(envKey); v != "" {
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
		if n, err := parseInt64(v); err == nil && n > 0 {
			return int(n)
		}
	}
	if configValue > 0 {
		return configValue
	}
	return defaultValue
}

// getConfigFloat64 returns the config value with priority: env var > config file > default.
func getConfigFloat64(envKey string, configValue float64, defaultValue float64) float64 {
	if v := os.Getenv(envKey); v != "" {
		if f, err := strconv.ParseFloat(v, 64); err == nil {
			return f
		}
	}
	if configValue > 0 {
		return configValue
	}
	return defaultValue
}

// getConfigBool returns the config value with priority: env var > config file > default.
func getConfigBool(envKey string, configValue bool, defaultValue bool) bool {
	if v := os.Getenv(envKey); v != "" {
		return v == "1" || v == "true"
	}
	if configValue {
		return true
	}
	return defaultValue
}
