package main

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"time"

	"codex-pool-proxy/internal/ratelimit"
)

// KimiProvider handles Kimi API accounts.
type KimiProvider struct {
	apiKeyUpstream
	bearerAuth
}

// NewKimiProvider creates a new Kimi provider.
func NewKimiProvider(kimiBase *url.URL) *KimiProvider {
	return &KimiProvider{apiKeyUpstream: apiKeyUpstream{base: kimiBase}}
}

func (p *KimiProvider) ModelDiscoveryURL() *url.URL {
	return modelDiscoveryURL(p.UpstreamURL("/v1/models"), "/v1/models")
}

func (p *KimiProvider) Type() AccountType {
	return AccountTypeKimi
}

// KimiAuthJSON is the format for Kimi auth files.
type KimiAuthJSON struct {
	APIKey string `json:"api_key"`
}

func (p *KimiProvider) LoadAccount(name, path string, data []byte) (*Account, error) {
	var kj KimiAuthJSON
	if err := json.Unmarshal(data, &kj); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if kj.APIKey == "" {
		return nil, nil
	}
	return newAPIKeyAccount(AccountTypeKimi, "kimi", name, path, kj.APIKey), nil
}

func (p *KimiProvider) ParseUsage(obj map[string]any) *RequestUsage {
	// Kimi proxies Anthropic/OpenAI-style responses, so parse both formats.

	// OpenAI-style usage object
	if usageMap, ok := obj["usage"].(map[string]any); ok {
		ru := &RequestUsage{Timestamp: time.Now(), InputTokenMode: "inclusive"}
		ru.InputTokens = readInt64(usageMap, "prompt_tokens")
		if ru.InputTokens == 0 {
			ru.InputTokens = readInt64(usageMap, "input_tokens")
		}
		ru.OutputTokens = readInt64(usageMap, "completion_tokens")
		if ru.OutputTokens == 0 {
			ru.OutputTokens = readInt64(usageMap, "output_tokens")
		}
		ru.CachedInputTokens = readInt64(usageMap, "cached_tokens")
		if ru.InputTokens == 0 && ru.OutputTokens == 0 {
			return nil
		}
		ru.BillableTokens = ru.InputTokens + ru.OutputTokens
		if m, ok := obj["model"].(string); ok && m != "" {
			ru.Model = m
		}
		return ru
	}

	return anthropicStreamUsage(obj)
}

func (p *KimiProvider) ParseUsageHeaders(acc *Account, headers http.Header) {
	snap, ok := parseKimiResponseRateLimits(headers)
	if !ok {
		return
	}
	acc.mu.Lock()
	acc.Usage = mergeUsage(acc.Usage, snap)
	acc.mu.Unlock()
}

// kimiModels lists model names that should be routed to the Kimi provider.
// Pi's built-in kimi-coding provider exposes both short aliases and provider-native IDs.
// isKimiModel returns true if the given model name should be routed to Kimi.
func isKimiModel(model string) bool {
	_, ok := modelForProvider(AccountTypeKimi, model)
	return ok
}

func parseKimiResponseRateLimits(headers http.Header) (UsageSnapshot, bool) {
	if headers == nil {
		return UsageSnapshot{}, false
	}

	snap := UsageSnapshot{
		RetrievedAt: time.Now(),
		Source:      "headers",
	}
	hasPrimary := false
	hasSecondary := false

	if pct, ok := ratelimit.ParsePercent(headers.Get("x-ratelimit-requests-utilization")); ok {
		snap.PrimaryUsedPercent = pct
		snap.PrimaryUsed = pct
		hasPrimary = true
	} else if pct, ok := ratelimit.UsageFromRemainingLimit(headers, "x-ratelimit-remaining-requests", "x-ratelimit-limit-requests"); ok {
		snap.PrimaryUsedPercent = pct
		snap.PrimaryUsed = pct
		hasPrimary = true
	} else if pct, ok := ratelimit.UsageFromRemainingLimit(headers, "x-ratelimit-requests-remaining", "x-ratelimit-requests-limit"); ok {
		snap.PrimaryUsedPercent = pct
		snap.PrimaryUsed = pct
		hasPrimary = true
	} else if pct, ok := ratelimit.UsageFromRemainingLimit(headers, "x-ratelimit-remaining", "x-ratelimit-limit"); ok {
		snap.PrimaryUsedPercent = pct
		snap.PrimaryUsed = pct
		hasPrimary = true
	}

	if pct, ok := ratelimit.ParsePercent(headers.Get("x-ratelimit-tokens-utilization")); ok {
		snap.SecondaryUsedPercent = pct
		snap.SecondaryUsed = pct
		hasSecondary = true
	} else if pct, ok := ratelimit.UsageFromRemainingLimit(headers, "x-ratelimit-remaining-tokens", "x-ratelimit-limit-tokens"); ok {
		snap.SecondaryUsedPercent = pct
		snap.SecondaryUsed = pct
		hasSecondary = true
	}

	if !hasPrimary && !hasSecondary {
		return UsageSnapshot{}, false
	}

	requestsReset, ok := ratelimit.ParseReset(headers.Get("x-ratelimit-reset-requests"))
	if !ok {
		requestsReset, ok = ratelimit.ParseReset(headers.Get("x-ratelimit-requests-reset"))
	}
	if ok {
		snap.PrimaryResetAt = requestsReset
	}

	tokensReset, ok := ratelimit.ParseReset(headers.Get("x-ratelimit-reset-tokens"))
	if !ok {
		tokensReset, ok = ratelimit.ParseReset(headers.Get("x-ratelimit-tokens-reset"))
	}
	if ok {
		snap.SecondaryResetAt = tokensReset
	}

	return snap, true
}
