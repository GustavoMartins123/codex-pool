package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"

	"codex-pool-proxy/internal/ratelimit"
)

// ClaudeProvider handles Anthropic Claude accounts.
type ClaudeProvider struct {
	claudeBase *url.URL
}

// NewClaudeProvider creates a new Claude provider.
func NewClaudeProvider(claudeBase *url.URL) *ClaudeProvider {
	return &ClaudeProvider{
		claudeBase: claudeBase,
	}
}

func (p *ClaudeProvider) ModelDiscoveryURL() *url.URL {
	return modelDiscoveryURL(p.UpstreamURL("/v1/models"), "/v1/models")
}

func (p *ClaudeProvider) SetModelDiscoveryHeaders(req *http.Request) {
	req.Header.Set("Anthropic-Version", "2023-06-01")
	req.Header.Set("Anthropic-Beta", "oauth-2025-04-20")
}

func (*ClaudeProvider) WireFormat() WireFormat { return WireAnthropic }

func (p *ClaudeProvider) Type() AccountType {
	return AccountTypeClaude
}

func (p *ClaudeProvider) LoadAccount(name, path string, data []byte) (*Account, error) {
	var cj ClaudeAuthJSON
	if err := json.Unmarshal(data, &cj); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}

	acc := &Account{
		Type: AccountTypeClaude,
		ID:   strings.TrimSuffix(name, filepath.Ext(name)),
		File: path,
	}

	// Load last_refresh from root level (for rate limiting across restarts)
	var root map[string]any
	if err := json.Unmarshal(data, &root); err == nil {
		if dead, ok := root["dead"].(bool); ok {
			acc.Dead = dead
		}
		if disabled, ok := root["disabled"].(bool); ok {
			acc.Disabled = disabled
		}
		if lr, ok := root["last_refresh"].(string); ok && lr != "" {
			if t, err := time.Parse(time.RFC3339Nano, lr); err == nil {
				acc.LastRefresh = t
			} else if t, err := time.Parse(time.RFC3339, lr); err == nil {
				acc.LastRefresh = t
			}
		}
	}
	if ip := strings.TrimSpace(cj.AllowedIP); ip != "" {
		acc.AllowedSourceIPs = append(acc.AllowedSourceIPs, ip)
	}
	for _, ip := range cj.AllowedSourceIPs {
		if ip = strings.TrimSpace(ip); ip != "" {
			acc.AllowedSourceIPs = append(acc.AllowedSourceIPs, ip)
		}
	}

	// Check for OAuth format first (from Claude Code keychain)
	if cj.ClaudeAiOauth != nil && cj.ClaudeAiOauth.AccessToken != "" {
		acc.AccessToken = cj.ClaudeAiOauth.AccessToken
		acc.RefreshToken = cj.ClaudeAiOauth.RefreshToken
		if cj.ClaudeAiOauth.ExpiresAt > 0 {
			acc.ExpiresAt = time.UnixMilli(cj.ClaudeAiOauth.ExpiresAt)
		}
		acc.PlanType = cj.ClaudeAiOauth.SubscriptionType
		if acc.PlanType == "" {
			acc.PlanType = "claude"
		}
		acc.RateLimitTier = cj.ClaudeAiOauth.RateLimitTier
		acc.AccountUUID = cj.AccountUUID
		return acc, nil
	}

	// Fall back to API key format
	if cj.APIKey == "" {
		return nil, nil
	}
	acc.AccessToken = cj.APIKey
	acc.PlanType = cj.PlanType
	if acc.PlanType == "" {
		acc.PlanType = "claude"
	}
	return acc, nil
}

func (p *ClaudeProvider) SetAuthHeaders(req *http.Request, acc *Account) {
	// OAuth tokens start with sk-ant-oat, API keys with sk-ant-api
	if strings.HasPrefix(acc.AccessToken, "sk-ant-oat") {
		req.Header.Set("Authorization", "Bearer "+acc.AccessToken)
	} else {
		req.Header.Set("X-Api-Key", acc.AccessToken)
	}
}

func (p *ClaudeProvider) RefreshToken(ctx context.Context, acc *Account, transport http.RoundTripper) error {
	if acc == nil {
		return errors.New("nil account")
	}
	acc.mu.Lock()
	access := acc.AccessToken
	acc.mu.Unlock()
	// Only OAuth tokens (not API keys) can be refreshed
	if !strings.HasPrefix(access, "sk-ant-oat") {
		// API keys don't need refresh
		return nil
	}

	return RefreshClaudeAccountTokensWithContext(ctx, acc, transport)
}

func (p *ClaudeProvider) ParseUsage(obj map[string]any) *RequestUsage {
	return anthropicStreamUsage(obj)
}

func (p *ClaudeProvider) ParseUsageHeaders(acc *Account, headers http.Header) {
	snap, ok := parseClaudeResponseRateLimits(headers)
	if !ok {
		return
	}
	acc.mu.Lock()
	acc.Usage = mergeUsage(acc.Usage, snap)
	acc.mu.Unlock()
	syncUsageCooldown(acc)
}

func (p *ClaudeProvider) UpstreamURL(path string) *url.URL {
	return p.claudeBase
}

func (p *ClaudeProvider) MatchesPath(path string) bool {
	return strings.HasPrefix(path, "/v1/messages")
}

func (p *ClaudeProvider) NormalizePath(path string) string {
	// Claude paths don't need normalization
	return path
}

func (p *ClaudeProvider) DetectsSSE(path string, contentType string) bool {
	// Claude uses text/event-stream content type for SSE
	return strings.Contains(strings.ToLower(contentType), "text/event-stream")
}

func parseClaudeResponseRateLimits(headers http.Header) (UsageSnapshot, bool) {
	if headers == nil {
		return UsageSnapshot{}, false
	}

	snap := UsageSnapshot{
		RetrievedAt: time.Now(),
		Source:      "headers",
	}
	usedPrimary := false
	usedSecondary := false

	primaryKeyChecks := []string{
		"anthropic-ratelimit-unified-5h-utilization",
		"anthropic-ratelimit-unified-primary-utilization",
		"anthropic-ratelimit-unified-tokens-utilization",
		"anthropic-ratelimit-tokens-utilization",
	}
	for _, key := range primaryKeyChecks {
		if pct, ok := ratelimit.ParsePercent(headers.Get(key)); ok {
			snap.PrimaryUsedPercent = pct
			snap.PrimaryUsed = pct
			usedPrimary = true
			break
		}
	}
	if !usedPrimary {
		if pct, ok := ratelimit.UsageFromRemainingLimit(headers, "anthropic-ratelimit-requests-remaining", "anthropic-ratelimit-requests-limit"); ok {
			snap.PrimaryUsedPercent = pct
			snap.PrimaryUsed = pct
			usedPrimary = true
		}
	}
	if !usedPrimary {
		if pct, ok := ratelimit.UsageFromRemainingLimit(headers, "x-ratelimit-remaining", "x-ratelimit-limit"); ok {
			snap.PrimaryUsedPercent = pct
			snap.PrimaryUsed = pct
			usedPrimary = true
		}
	}

	secondaryKeyChecks := []string{
		"anthropic-ratelimit-unified-7d-utilization",
		"anthropic-ratelimit-unified-secondary-utilization",
		"anthropic-ratelimit-unified-requests-utilization",
		"anthropic-ratelimit-requests-utilization",
	}
	for _, key := range secondaryKeyChecks {
		if pct, ok := ratelimit.ParsePercent(headers.Get(key)); ok {
			snap.SecondaryUsedPercent = pct
			snap.SecondaryUsed = pct
			usedSecondary = true
			break
		}
	}
	if !usedSecondary {
		if pct, ok := ratelimit.UsageFromRemainingLimit(headers, "anthropic-ratelimit-tokens-remaining", "anthropic-ratelimit-tokens-limit"); ok {
			snap.SecondaryUsedPercent = pct
			snap.SecondaryUsed = pct
			usedSecondary = true
		}
	}
	if !usedSecondary {
		if pct, ok := ratelimit.UsageFromRemainingLimit(headers, "x-ratelimit-remaining-requests", "x-ratelimit-limit-requests"); ok {
			snap.SecondaryUsedPercent = pct
			snap.SecondaryUsed = pct
			usedSecondary = true
		}
	}
	if !usedSecondary {
		if pct, ok := ratelimit.UsageFromRemainingLimit(headers, "x-ratelimit-remaining-tokens", "x-ratelimit-limit-tokens"); ok {
			snap.SecondaryUsedPercent = pct
			snap.SecondaryUsed = pct
			usedSecondary = true
		}
	}

	if !usedPrimary && !usedSecondary {
		return UsageSnapshot{}, false
	}

	if resetStr := headers.Get("anthropic-ratelimit-unified-primary-reset"); resetStr != "" {
		if resetAt, ok := ratelimit.ParseReset(resetStr); ok {
			snap.PrimaryResetAt = resetAt
		}
	} else if resetStr := headers.Get("anthropic-ratelimit-unified-5h-reset"); resetStr != "" {
		if resetAt, ok := ratelimit.ParseReset(resetStr); ok {
			snap.PrimaryResetAt = resetAt
		}
	} else if resetStr := headers.Get("anthropic-ratelimit-unified-reset"); resetStr != "" {
		if resetAt, ok := ratelimit.ParseReset(resetStr); ok {
			snap.PrimaryResetAt = resetAt
		}
	} else if resetStr := headers.Get("anthropic-ratelimit-unified-tokens-reset"); resetStr != "" {
		if resetAt, ok := ratelimit.ParseReset(resetStr); ok {
			snap.PrimaryResetAt = resetAt
		}
	} else if resetStr := headers.Get("anthropic-ratelimit-tokens-reset"); resetStr != "" {
		if resetAt, ok := ratelimit.ParseReset(resetStr); ok {
			snap.PrimaryResetAt = resetAt
		}
	} else if resetStr := headers.Get("anthropic-ratelimit-requests-reset"); resetStr != "" {
		if resetAt, ok := ratelimit.ParseReset(resetStr); ok {
			snap.PrimaryResetAt = resetAt
		}
	} else if resetStr := headers.Get("x-ratelimit-reset"); resetStr != "" {
		if resetAt, ok := ratelimit.ParseReset(resetStr); ok {
			snap.PrimaryResetAt = resetAt
		}
	}

	if resetStr := headers.Get("anthropic-ratelimit-unified-secondary-reset"); resetStr != "" {
		if resetAt, ok := ratelimit.ParseReset(resetStr); ok {
			snap.SecondaryResetAt = resetAt
		}
	} else if resetStr := headers.Get("anthropic-ratelimit-unified-requests-reset"); resetStr != "" {
		if resetAt, ok := ratelimit.ParseReset(resetStr); ok {
			snap.SecondaryResetAt = resetAt
		}
	} else if resetStr := headers.Get("anthropic-ratelimit-unified-7d-reset"); resetStr != "" {
		if resetAt, ok := ratelimit.ParseReset(resetStr); ok {
			snap.SecondaryResetAt = resetAt
		}
	} else if resetStr := headers.Get("anthropic-ratelimit-unified-reset"); resetStr != "" {
		if resetAt, ok := ratelimit.ParseReset(resetStr); ok {
			snap.SecondaryResetAt = resetAt
		}
	} else if resetStr := headers.Get("anthropic-ratelimit-requests-reset"); resetStr != "" {
		if resetAt, ok := ratelimit.ParseReset(resetStr); ok {
			snap.SecondaryResetAt = resetAt
		}
	} else if resetStr := headers.Get("x-ratelimit-reset"); resetStr != "" {
		if resetAt, ok := ratelimit.ParseReset(resetStr); ok {
			snap.SecondaryResetAt = resetAt
		}
	}

	snap.primarySet, snap.PrimaryUsageReported = usedPrimary, usedPrimary
	snap.secondarySet, snap.SecondaryUsageReported = usedSecondary, usedSecondary
	return snap, true
}
