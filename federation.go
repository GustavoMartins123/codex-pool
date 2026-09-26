package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"sort"
	"strings"
	"sync"
	"time"
)

type FederationNodeConfig struct {
	BaseURL             string `toml:"base_url"`
	TokenEnv            string `toml:"token_env"`
	Enabled             bool   `toml:"enabled"`
	PollIntervalSeconds int    `toml:"poll_interval_seconds"`
}

type FederationConfig struct {
	Enabled bool                            `toml:"enabled"`
	Nodes   map[string]FederationNodeConfig `toml:"nodes"`
}

type FederatedProvider struct {
	name        string
	accountType AccountType
	base        *url.URL
	tokenEnv    string
	interval    time.Duration
	mu          sync.RWMutex
	models      map[string]string
}

func NewFederatedProvider(name string, cfg FederationNodeConfig) (*FederatedProvider, error) {
	name = strings.TrimSpace(name)
	if name == "" || strings.ContainsAny(name, " \t\r\n/") {
		return nil, errors.New("federation node name must be a non-empty path-safe identifier")
	}
	base, err := url.Parse(strings.TrimSpace(cfg.BaseURL))
	if err != nil || base.Scheme == "" || base.Host == "" {
		return nil, fmt.Errorf("federation node %s: invalid base_url", name)
	}
	interval := time.Duration(cfg.PollIntervalSeconds) * time.Second
	if interval <= 0 {
		interval = 30 * time.Second
	}
	provider := &FederatedProvider{
		name: name, accountType: AccountType("federation:" + name), base: base,
		tokenEnv: strings.TrimSpace(cfg.TokenEnv), interval: interval, models: make(map[string]string),
	}
	markGenericProviderType(provider.accountType)
	return provider, nil
}

func (p *FederatedProvider) Type() AccountType { return p.accountType }
func (p *FederatedProvider) LoadAccount(string, string, []byte) (*Account, error) {
	return nil, nil
}
func (p *FederatedProvider) SetAuthHeaders(request *http.Request, _ *Account) {
	request.Header.Set("Authorization", "Bearer "+os.Getenv(p.tokenEnv))
	request.Header.Set("X-Pool-Federation-Node", p.name)
}
func (p *FederatedProvider) RefreshToken(context.Context, *Account, http.RoundTripper) error {
	return nil
}
func (p *FederatedProvider) ParseUsage(object map[string]any) *RequestUsage {
	return (&GenericOpenAIProvider{}).ParseUsage(object)
}
func (p *FederatedProvider) ParseUsageHeaders(*Account, http.Header) {}
func (p *FederatedProvider) UpstreamURL(string) *url.URL             { return p.base }
func (p *FederatedProvider) MatchesPath(string) bool                 { return false }
func (p *FederatedProvider) NormalizePath(path string) string {
	basePath := strings.TrimRight(p.base.Path, "/")
	if basePath != "" && strings.HasPrefix(path, basePath+"/") {
		return strings.TrimPrefix(path, basePath)
	}
	return path
}
func (p *FederatedProvider) DetectsSSE(_ string, contentType string) bool {
	return strings.Contains(strings.ToLower(contentType), "text/event-stream")
}
func (p *FederatedProvider) ResolveModel(model string) (string, bool) {
	p.mu.RLock()
	canonical, ok := p.models[strings.ToLower(strings.TrimSpace(model))]
	p.mu.RUnlock()
	return canonical, ok
}
func (p *FederatedProvider) syntheticAccount() *Account {
	token := os.Getenv(p.tokenEnv)
	account := &Account{
		Type: p.accountType, ID: "federation-" + p.name, Label: p.name,
		AccessToken: token, PlanType: "federated", Models: make(map[string]DiscoveredModel),
	}
	if token == "" {
		account.Disabled = true
		account.HealthError = "federation token is unavailable"
	}
	return account
}

func buildFederatedProviders(cfg FederationConfig) ([]Provider, []*Account, error) {
	if !cfg.Enabled {
		return nil, nil, nil
	}
	names := make([]string, 0, len(cfg.Nodes))
	for name, node := range cfg.Nodes {
		if node.Enabled {
			names = append(names, name)
		}
	}
	sort.Strings(names)
	var providers []Provider
	var accounts []*Account
	for _, name := range names {
		provider, err := NewFederatedProvider(name, cfg.Nodes[name])
		if err != nil {
			return nil, nil, err
		}
		providers = append(providers, provider)
		accounts = append(accounts, provider.syntheticAccount())
	}
	return providers, accounts, nil
}

func configuredFederatedAccounts(registry *ProviderRegistry) []*Account {
	if registry == nil {
		return nil
	}
	var accounts []*Account
	for _, registered := range registry.All() {
		if provider, ok := registered.(*FederatedProvider); ok {
			accounts = append(accounts, provider.syntheticAccount())
		}
	}
	return accounts
}

func (p *FederatedProvider) syncModels(ctx context.Context, transport http.RoundTripper, account *Account) error {
	target := *p.base
	target.Path = singleJoin(target.Path, "/api/pool/models")
	request, err := http.NewRequestWithContext(ctx, http.MethodGet, target.String(), nil)
	if err != nil {
		return err
	}
	p.SetAuthHeaders(request, account)
	response, err := transport.RoundTrip(request)
	if err != nil {
		return err
	}
	defer response.Body.Close()
	body, err := io.ReadAll(io.LimitReader(response.Body, 8<<20))
	if err != nil {
		return err
	}
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		return fmt.Errorf("federation catalog returned %s: %s", response.Status, safeText(body))
	}
	var catalog struct {
		Models []poolModelDescriptor `json:"models"`
	}
	if err := json.Unmarshal(body, &catalog); err != nil {
		return err
	}
	if len(catalog.Models) == 0 {
		return errors.New("federation node announced no models")
	}
	models := make(map[string]DiscoveredModel, len(catalog.Models))
	routes := make(map[string]string, len(catalog.Models))
	for _, remote := range catalog.Models {
		if strings.TrimSpace(remote.ID) == "" || !remote.AvailableNow {
			continue
		}
		publicID := "federation/" + p.name + "/" + remote.ID
		models[publicID] = DiscoveredModel{
			ID: publicID, DisplayName: firstNonEmpty(remote.Name, remote.ID),
			Description: remote.Description, ContextWindow: remote.ContextWindow,
			MaxOutputTokens: remote.MaxOutputTokens,
			Reasoning:       remote.Capabilities["reasoning"], WebSearch: remote.Capabilities["web_search"],
			Modalities: append([]string(nil), remote.Modalities...), Protocol: remote.Protocol,
		}
		routes[strings.ToLower(publicID)] = remote.ID
	}
	if len(models) == 0 {
		return errors.New("federation node has no currently available models")
	}
	p.mu.Lock()
	p.models = routes
	p.mu.Unlock()
	account.mu.Lock()
	account.Models = models
	account.ModelsFetchedAt = time.Now().UTC()
	account.HealthError = ""
	account.Dead = false
	account.Disabled = false
	account.mu.Unlock()
	return nil
}

func (h *proxyHandler) startFederationPoller(ctx ...context.Context) {
	c := context.Background()
	if len(ctx) > 0 && ctx[0] != nil {
		c = ctx[0]
	}
	if h == nil || h.registry == nil || h.pool == nil {
		return
	}
	for _, registered := range h.registry.All() {
		provider, ok := registered.(*FederatedProvider)
		if !ok {
			continue
		}
		var account *Account
		for _, candidate := range h.pool.allAccounts() {
			if candidate.Type == provider.Type() && candidate.ID == "federation-"+provider.name {
				account = candidate
				break
			}
		}
		if account == nil || account.AccessToken == "" {
			continue
		}
		go func(provider *FederatedProvider, account *Account) {
			sync := func() {
				syncCtx, cancel := context.WithTimeout(c, 10*time.Second)
				err := provider.syncModels(syncCtx, h.transport, account)
				cancel()
				updateFederationHealth(account, err)
			}
			sync()
			ticker := time.NewTicker(provider.interval)
			defer ticker.Stop()
			for {
				select {
				case <-c.Done():
					return
				case <-ticker.C:
				sync()
				}
			}
		}(provider, account)
	}
}

func updateFederationHealth(account *Account, err error) {
	if account == nil || err == nil {
		return
	}
	account.mu.Lock()
	account.HealthError = err.Error()
	account.Dead = true
	account.mu.Unlock()
}
