package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type GenericProviderAuthConfig struct {
	Type     string `toml:"type"`
	TokenEnv string `toml:"token_env"`
	Header   string `toml:"header"`
	Prefix   string `toml:"prefix"`
}

type GenericProviderHealthConfig struct {
	Path     string `toml:"path"`
	Interval int    `toml:"interval_seconds"`
}

type GenericProviderModelConfig struct {
	ID              string   `toml:"id"`
	Name            string   `toml:"name"`
	Aliases         []string `toml:"aliases"`
	ContextWindow   int      `toml:"context_window"`
	MaxOutputTokens int      `toml:"max_output_tokens"`
	Reasoning       bool     `toml:"reasoning"`
	WebSearch       bool     `toml:"web_search"`
	Modalities      []string `toml:"modalities"`
	Fallbacks       []string `toml:"fallbacks"`
	FallbackFor     []string `toml:"fallback_for"`
}

type GenericProviderConfig struct {
	Protocol string                       `toml:"protocol"`
	BaseURL  string                       `toml:"base_url"`
	Auth     GenericProviderAuthConfig    `toml:"auth"`
	Health   GenericProviderHealthConfig  `toml:"health"`
	Models   []GenericProviderModelConfig `toml:"models"`
}

type GenericOpenAIProvider struct {
	name        string
	accountType AccountType
	base        *url.URL
	auth        GenericProviderAuthConfig
	health      GenericProviderHealthConfig
	models      map[string]GenericProviderModelConfig
	publicIDs   []string
}

var genericProviderTypes = struct {
	sync.RWMutex
	types map[AccountType]bool
}{types: make(map[AccountType]bool)}

func markGenericProviderType(accountType AccountType) {
	genericProviderTypes.Lock()
	genericProviderTypes.types[accountType] = true
	genericProviderTypes.Unlock()
}

func isGenericProviderType(accountType AccountType) bool {
	genericProviderTypes.RLock()
	ok := genericProviderTypes.types[accountType]
	genericProviderTypes.RUnlock()
	return ok
}

func NewGenericOpenAIProvider(name string, cfg GenericProviderConfig) (*GenericOpenAIProvider, error) {
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, " \t\r\n/") {
		return nil, errors.New("generic provider name must be a non-empty path-safe identifier")
	}
	for _, reserved := range []AccountType{
		AccountTypeCodex, AccountTypeClaude, AccountTypeGemini, AccountTypeAntigravity,
		AccountTypeKimi, AccountTypeMinimax, AccountTypeZAI, AccountTypeXiaomi,
		AccountTypeGrok, AccountTypeAdverserial, AccountTypeOpencodeGo,
	} {
		if strings.EqualFold(name, string(reserved)) {
			return nil, fmt.Errorf("generic provider name %q conflicts with a built-in provider", name)
		}
	}
	if protocol := strings.ToLower(strings.TrimSpace(cfg.Protocol)); protocol != "" && protocol != "openai" {
		return nil, fmt.Errorf("generic provider %s: unsupported protocol %q", name, cfg.Protocol)
	}
	base, err := url.Parse(strings.TrimSpace(cfg.BaseURL))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("generic provider %s: invalid base_url", name)
	}
	provider := &GenericOpenAIProvider{
		name: name, accountType: AccountType(name), base: base,
		auth: cfg.Auth, health: cfg.Health, models: make(map[string]GenericProviderModelConfig),
	}
	for _, model := range cfg.Models {
		model.ID = strings.TrimSpace(model.ID)
		if model.ID == "" {
			return nil, fmt.Errorf("generic provider %s: model id is required", name)
		}
		if model.Name == "" {
			model.Name = model.ID
		}
		if model.ContextWindow <= 0 {
			model.ContextWindow = 128000
		}
		if model.MaxOutputTokens <= 0 {
			model.MaxOutputTokens = 32768
		}
		if len(model.Modalities) == 0 {
			model.Modalities = []string{"text"}
		}
		publicID := name + "/" + model.ID
		provider.publicIDs = append(provider.publicIDs, publicID)
		provider.models[strings.ToLower(publicID)] = model
		for _, alias := range model.Aliases {
			alias = strings.ToLower(strings.TrimSpace(alias))
			if alias != "" {
				provider.models[alias] = model
			}
		}
	}
	sort.Strings(provider.publicIDs)
	markGenericProviderType(provider.accountType)
	return provider, nil
}

func (p *GenericOpenAIProvider) Type() AccountType { return p.accountType }

func (p *GenericOpenAIProvider) LoadAccount(name, path string, data []byte) (*Account, error) {
	return nil, nil
}

func (p *GenericOpenAIProvider) SetAuthHeaders(req *http.Request, acc *Account) {
	token := ""
	if acc != nil {
		token = acc.AccessToken
	}
	authType := strings.ToLower(strings.TrimSpace(p.auth.Type))
	switch authType {
	case "", "bearer":
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
	case "header", "api-key":
		header := strings.TrimSpace(p.auth.Header)
		if header == "" {
			header = "X-Api-Key"
		}
		req.Header.Set(header, p.auth.Prefix+token)
	case "none":
	}
}

func (p *GenericOpenAIProvider) RefreshToken(context.Context, *Account, http.RoundTripper) error {
	return nil
}

func (p *GenericOpenAIProvider) ParseUsage(obj map[string]any) *RequestUsage {
	usage, _ := obj["usage"].(map[string]any)
	if usage == nil {
		if response, _ := obj["response"].(map[string]any); response != nil {
			usage, _ = response["usage"].(map[string]any)
		}
	}
	if usage == nil {
		return nil
	}
	result := &RequestUsage{Timestamp: time.Now(), InputTokenMode: "inclusive"}
	result.InputTokens = readInt64(usage, "input_tokens")
	if result.InputTokens == 0 {
		result.InputTokens = readInt64(usage, "prompt_tokens")
	}
	result.OutputTokens = readInt64(usage, "output_tokens")
	if result.OutputTokens == 0 {
		result.OutputTokens = readInt64(usage, "completion_tokens")
	}
	result.CachedInputTokens = readInt64(usage, "cached_tokens")
	if details, _ := usage["input_tokens_details"].(map[string]any); details != nil {
		result.CachedInputTokens = readInt64(details, "cached_tokens")
	}
	if result.InputTokens == 0 && result.OutputTokens == 0 {
		return nil
	}
	result.BillableTokens = clampNonNegative(result.InputTokens - result.CachedInputTokens + result.OutputTokens)
	if model, _ := obj["model"].(string); model != "" {
		result.Model = model
	}
	return result
}

func (p *GenericOpenAIProvider) ParseUsageHeaders(*Account, http.Header) {}
func (p *GenericOpenAIProvider) UpstreamURL(string) *url.URL             { return p.base }
func (p *GenericOpenAIProvider) MatchesPath(string) bool                 { return false }

func (p *GenericOpenAIProvider) NormalizePath(path string) string {
	basePath := strings.TrimRight(p.base.Path, "/")
	if basePath != "" && strings.HasPrefix(path, basePath+"/") {
		return strings.TrimPrefix(path, basePath)
	}
	return path
}

func (p *GenericOpenAIProvider) DetectsSSE(_ string, contentType string) bool {
	return strings.Contains(strings.ToLower(contentType), "text/event-stream")
}

func (p *GenericOpenAIProvider) ResolveModel(model string) (string, bool) {
	entry, ok := p.models[strings.ToLower(strings.TrimSpace(model))]
	return entry.ID, ok
}

func (p *GenericOpenAIProvider) configuredModels() map[string]DiscoveredModel {
	out := make(map[string]DiscoveredModel, len(p.publicIDs))
	for _, publicID := range p.publicIDs {
		model := p.models[strings.ToLower(publicID)]
		out[publicID] = DiscoveredModel{
			ID: publicID, DisplayName: model.Name, ContextWindow: model.ContextWindow,
			MaxOutputTokens: model.MaxOutputTokens, Reasoning: model.Reasoning,
			WebSearch: model.WebSearch, Modalities: append([]string(nil), model.Modalities...),
			Protocol: "openai",
		}
	}
	return out
}

func (p *GenericOpenAIProvider) syntheticAccount() *Account {
	token := ""
	if env := strings.TrimSpace(p.auth.TokenEnv); env != "" {
		token = os.Getenv(env)
	}
	account := &Account{
		Type: p.accountType, ID: "configured-" + p.name, Label: p.name,
		AccessToken: token, PlanType: "configured", Models: p.configuredModels(),
		ModelsFetchedAt: time.Now().UTC(),
	}
	if strings.ToLower(strings.TrimSpace(p.auth.Type)) != "none" && token == "" {
		account.HealthError = "configured auth token is unavailable"
		account.Disabled = true
	}
	return account
}

func buildGenericProviders(configured map[string]GenericProviderConfig) ([]Provider, []*Account, error) {
	names := make([]string, 0, len(configured))
	for name := range configured {
		names = append(names, name)
	}
	sort.Strings(names)
	providers := make([]Provider, 0, len(names))
	accounts := make([]*Account, 0, len(names))
	for _, name := range names {
		provider, err := NewGenericOpenAIProvider(name, configured[name])
		if err != nil {
			return nil, nil, err
		}
		providers = append(providers, provider)
		accounts = append(accounts, provider.syntheticAccount())
	}
	return providers, accounts, nil
}

func configuredGenericAccounts(registry *ProviderRegistry) []*Account {
	if registry == nil {
		return nil
	}
	var accounts []*Account
	for _, registered := range registry.All() {
		if provider, ok := registered.(*GenericOpenAIProvider); ok {
			accounts = append(accounts, provider.syntheticAccount())
		}
	}
	return accounts
}

func (p *GenericOpenAIProvider) healthURL() *url.URL {
	target := *p.base
	path := strings.TrimSpace(p.health.Path)
	if path == "" {
		path = "/models"
	}
	target.Path = singleJoin(target.Path, path)
	return &target
}

func (h *proxyHandler) startGenericProviderHealthPoller() {
	if h == nil || h.registry == nil || h.pool == nil {
		return
	}
	for _, registered := range h.registry.All() {
		provider, ok := registered.(*GenericOpenAIProvider)
		if !ok {
			continue
		}
		var account *Account
		for _, candidate := range h.pool.allAccounts() {
			if candidate.ID == "configured-"+provider.name && candidate.Type == provider.Type() {
				account = candidate
				break
			}
		}
		if account == nil {
			continue
		}
		interval := time.Duration(provider.health.Interval) * time.Second
		if interval <= 0 {
			interval = 30 * time.Second
		}
		go func(provider *GenericOpenAIProvider, account *Account) {
			check := func() {
				ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
				defer cancel()
				req, err := http.NewRequestWithContext(ctx, http.MethodGet, provider.healthURL().String(), nil)
				if err == nil {
					provider.SetAuthHeaders(req, account)
					var response *http.Response
					response, err = h.transport.RoundTrip(req)
					if response != nil {
						response.Body.Close()
						if response.StatusCode < 200 || response.StatusCode >= 400 {
							err = fmt.Errorf("health endpoint returned %s", response.Status)
						}
					}
				}
				account.mu.Lock()
				if err != nil {
					account.HealthError = err.Error()
					account.Dead = true
				} else {
					account.HealthError = ""
					account.Dead = false
				}
				account.mu.Unlock()
			}
			check()
			ticker := time.NewTicker(interval)
			defer ticker.Stop()
			for range ticker.C {
				check()
			}
		}(provider, account)
	}
}

func genericProviderConfigJSON(config GenericProviderConfig) []byte {
	safe := config
	safe.Auth.TokenEnv = ""
	data, _ := json.Marshal(safe)
	return data
}
