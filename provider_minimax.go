package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"
)

// MinimaxProvider handles MiniMax API accounts.
type MinimaxProvider struct {
	apiKeyUpstream
	bearerAuth
}

// NewMinimaxProvider creates a new MiniMax provider.
func NewMinimaxProvider(minimaxBase *url.URL) *MinimaxProvider {
	return &MinimaxProvider{apiKeyUpstream: apiKeyUpstream{base: minimaxBase}}
}

func (p *MinimaxProvider) ModelDiscoveryURL() *url.URL {
	return modelDiscoveryURL(p.UpstreamURL("/v1/models"), "/v1/models")
}

func (p *MinimaxProvider) Type() AccountType {
	return AccountTypeMinimax
}

// MinimaxAuthJSON is the format for MiniMax auth files.
type MinimaxAuthJSON struct {
	APIKey string `json:"api_key"`
}

func (p *MinimaxProvider) LoadAccount(name, path string, data []byte) (*Account, error) {
	var mj MinimaxAuthJSON
	if err := json.Unmarshal(data, &mj); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if mj.APIKey == "" {
		return nil, nil
	}
	return newAPIKeyAccount(AccountTypeMinimax, "minimax", name, path, mj.APIKey), nil
}

func (p *MinimaxProvider) ParseUsage(obj map[string]any) *RequestUsage {
	// MiniMax uses the Anthropic API format since the base URL is /anthropic
	return anthropicStreamUsage(obj)
}

func (p *MinimaxProvider) ParseUsageHeaders(acc *Account, headers http.Header) {
	applyMinimaxRateLimits(acc, headers, time.Now())
}

// minimaxModels maps request model names to the canonical model name sent upstream.
// isMinimaxModel returns true if the given model name should be routed to MiniMax.
func isMinimaxModel(model string) bool {
	_, ok := modelForProvider(AccountTypeMinimax, model)
	return ok
}

// minimaxCanonicalModel returns the canonical upstream model name for a MiniMax alias.
func minimaxCanonicalModel(model string) string {
	if found, ok := modelForProvider(AccountTypeMinimax, model); ok {
		return found.ID
	}
	return model
}
