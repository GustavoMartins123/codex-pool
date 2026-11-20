package main

import (
	"context"
	"net/http"
	"net/url"
)

// Provider describes how to reach an upstream.
type Provider interface {
	Type() AccountType
	MatchesPath(path string) bool
	UpstreamURL(path string) *url.URL
}

// Authenticator loads account credentials and authenticates upstream requests.
type Authenticator interface {
	LoadAccount(name, path string, data []byte) (*Account, error)
	SetAuthHeaders(req *http.Request, acc *Account)
	RefreshToken(ctx context.Context, acc *Account, transport http.RoundTripper) error
}

// UsageParser reads token usage from a provider response event.
type UsageParser interface {
	ParseUsage(event map[string]any) *RequestUsage
}

// RateLimitHeaderParser is implemented by upstreams that report quota in headers.
type RateLimitHeaderParser interface {
	ParseUsageHeaders(account *Account, headers http.Header)
}

// Codec describes the HTTP framing of a provider protocol.
type Codec interface {
	WireFormat() WireFormat
	NormalizePath(path string) string
	DetectsSSE(path string, contentType string) bool
}

// ProviderDriver composes the contracts required by the proxy.
type ProviderDriver interface {
	Provider
	Authenticator
	UsageParser
	Codec
}

// ModelResolver routes a configured model name to its upstream model ID.
type ModelResolver interface {
	ResolveModel(model string) (string, bool)
}

type registeredModelResolver struct {
	provider ProviderDriver
	resolver ModelResolver
}

// ProviderRegistry manages all provider implementations.
type ProviderRegistry struct {
	providers           []ProviderDriver
	byType              map[AccountType]ProviderDriver
	modelResolvers      []registeredModelResolver
	bufferedModelRoutes []providerModelRoute
	streamedModelRoutes []providerModelRoute
}

// NewProviderRegistry creates a registry with all configured providers.
// Order matters for path matching: more specific patterns must come first.
// Claude (/v1/messages) must be checked before Codex (/v1/) to avoid false matches.
// Extra providers (e.g., Kimi, MiniMax) are model-routed and never win path matching.
func NewProviderRegistry(codex *CodexProvider, claude *ClaudeProvider, gemini *GeminiProvider, extra ...ProviderDriver) *ProviderRegistry {
	// Order: Gemini (unique paths), Claude (specific /v1/messages), Codex (broad /v1/)
	providers := make([]ProviderDriver, 0, 3+len(extra))
	if gemini != nil {
		providers = append(providers, gemini)
	}
	if claude != nil {
		providers = append(providers, claude)
	}
	if codex != nil {
		providers = append(providers, codex)
	}
	providers = append(providers, extra...)
	registry := &ProviderRegistry{providers: providers, byType: make(map[AccountType]ProviderDriver, len(providers))}
	for _, p := range providers {
		registry.byType[p.Type()] = p
		if resolver, ok := p.(ModelResolver); ok {
			registry.modelResolvers = append(registry.modelResolvers, registeredModelResolver{provider: p, resolver: resolver})
		}
	}
	registry.bufferedModelRoutes, registry.streamedModelRoutes = configuredModelRoutes()
	return registry
}

// ForType returns the provider for the given account type.
func (r *ProviderRegistry) ForType(t AccountType) ProviderDriver {
	return r.byType[t]
}

// ForPath returns the provider that handles the given request path.
// Returns nil if no provider matches.
func (r *ProviderRegistry) ForPath(path string) ProviderDriver {
	for _, p := range r.providers {
		if p.MatchesPath(path) {
			return p
		}
	}
	return nil
}

// All returns all registered providers.
func (r *ProviderRegistry) All() []ProviderDriver {
	return r.providers
}

// ResolveModel returns a configuration-driven provider and its upstream model ID.
func (r *ProviderRegistry) ResolveModel(model string) (ProviderDriver, string, bool) {
	for _, registered := range r.modelResolvers {
		if canonical, matched := registered.resolver.ResolveModel(model); matched {
			return registered.provider, canonical, true
		}
	}
	return nil, "", false
}
