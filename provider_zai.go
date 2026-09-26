package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

// ZAIProvider handles Z.ai GLM Coding Plan accounts through the Anthropic-compatible API.
type ZAIProvider struct {
	zaiBase *url.URL
}

// NewZAIProvider creates a new Z.ai provider.
func NewZAIProvider(zaiBase *url.URL) *ZAIProvider {
	return &ZAIProvider{
		zaiBase: zaiBase,
	}
}

func (p *ZAIProvider) Type() AccountType {
	return AccountTypeZAI
}

type ZAIAuthJSON struct {
	APIKey           string   `json:"api_key"`
	AuthType         string   `json:"auth_type,omitempty"`
	UserID           string   `json:"user_id,omitempty"`
	BusinessToken    string   `json:"business_token,omitempty"`
	ZCodeJWT         string   `json:"zcode_jwt,omitempty"`
	LinkedAt         string   `json:"linked_at,omitempty"`
	AddedAt          string   `json:"added_at,omitempty"`
	PlanType         string   `json:"plan_type,omitempty"`
	Label            string   `json:"label,omitempty"`
	Email            string   `json:"email,omitempty"`
	MonthlyCost      *float64 `json:"monthly_cost,omitempty"`
	SubscriptionCost *float64 `json:"subscription_cost,omitempty"`
	LimitRPM         int      `json:"limit_rpm,omitempty"`
	LimitTPM         int      `json:"limit_tpm,omitempty"`
	DailyTokenLimit  int64    `json:"daily_token_limit,omitempty"`
	WindowMinutes    int      `json:"window_minutes,omitempty"`
	RateLimitTier    string   `json:"rate_limit_tier,omitempty"`
	AllowedSourceIPs []string `json:"allowed_source_ips,omitempty"`
	Dead             bool     `json:"dead,omitempty"`
	Disabled         bool     `json:"disabled,omitempty"`
}

func (p *ZAIProvider) LoadAccount(name, path string, data []byte) (*Account, error) {
	var zj ZAIAuthJSON
	if err := json.Unmarshal(data, &zj); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	if zj.AuthType == "oauth" && (zj.APIKey == "" || zj.UserID == "" || zj.BusinessToken == "" || zj.ZCodeJWT == "" || zj.PlanType != "coding_plan") {
		return nil, fmt.Errorf("parse %s: incomplete Z.ai OAuth account", path)
	}
	if zj.AuthType != "" && zj.AuthType != "oauth" {
		return nil, fmt.Errorf("parse %s: unsupported Z.ai auth type %q", path, zj.AuthType)
	}
	if zj.APIKey == "" {
		return nil, nil
	}

	planType := strings.TrimSpace(zj.PlanType)
	if planType == "" || planType == "zai" {
		planType = "coding_plan"
	}
	label := strings.TrimSpace(zj.Label)
	if label == "" {
		label = "Z.ai Coding Plan"
	}

	monthlyCost := float64(15.0)
	if zj.MonthlyCost != nil && *zj.MonthlyCost >= 0 {
		monthlyCost = *zj.MonthlyCost
	} else if zj.SubscriptionCost != nil && *zj.SubscriptionCost >= 0 {
		monthlyCost = *zj.SubscriptionCost
	}

	// Coding Plan quota is not a fixed midnight daily window. Keep legacy
	// explicit configuration metadata, but never synthesize usage percentages
	// or reset timestamps when Z.ai has not reported them.
	usage := UsageSnapshot{
		RetrievedAt: time.Now(),
		Source:      "config",
	}
	if zj.WindowMinutes > 0 {
		usage.PrimaryWindowMinutes = zj.WindowMinutes
	}

	acc := &Account{
		Type:             AccountTypeZAI,
		ID:               strings.TrimSuffix(name, filepath.Ext(name)),
		File:             path,
		Label:            label,
		Email:            zj.Email,
		AccessToken:      zj.APIKey,
		PlanType:         planType,
		MonthlyCost:      monthlyCost,
		DailyTokenLimit:  zj.DailyTokenLimit,
		RateLimitTier:    zj.RateLimitTier,
		AllowedSourceIPs: zj.AllowedSourceIPs,
		Dead:             zj.Dead,
		Disabled:         zj.Disabled,
		Usage:            usage,
	}
	return acc, nil
}
func (p *ZAIProvider) SetAuthHeaders(req *http.Request, acc *Account) {
	req.Header.Set("X-Api-Key", acc.AccessToken)
	req.Header.Set("Authorization", "Bearer "+acc.AccessToken)
	if req.Header.Get("anthropic-version") == "" {
		req.Header.Set("anthropic-version", ccAnthropicVersion)
	}
}

func (p *ZAIProvider) RefreshToken(ctx context.Context, acc *Account, transport http.RoundTripper) error {
	return nil
}

func (p *ZAIProvider) ParseUsage(obj map[string]any) *RequestUsage {
	if obj == nil {
		return nil
	}

	// 1. OpenAI-style usage object (e.g. from chat completions)
	if usageMap, ok := obj["usage"].(map[string]any); ok && usageMap != nil {
		ru := &RequestUsage{Timestamp: time.Now(), InputTokenMode: "inclusive"}
		ru.InputTokens = readInt64(usageMap, "prompt_tokens")
		if ru.InputTokens == 0 {
			ru.InputTokens = readInt64(usageMap, "input_tokens")
		}
		ru.OutputTokens = readInt64(usageMap, "completion_tokens")
		if ru.OutputTokens == 0 {
			ru.OutputTokens = readInt64(usageMap, "output_tokens")
		}
		ru.CachedInputTokens = readInt64(usageMap, "cache_read_input_tokens")
		if ru.CachedInputTokens == 0 {
			ru.CachedInputTokens = readInt64(usageMap, "cached_tokens")
			if ru.CachedInputTokens == 0 {
				if details, ok := usageMap["prompt_tokens_details"].(map[string]any); ok {
					ru.CachedInputTokens = readInt64(details, "cached_tokens")
				}
			}
		}
		if details, ok := usageMap["completion_tokens_details"].(map[string]any); ok {
			ru.ReasoningTokens = readInt64(details, "reasoning_tokens")
		}
		if m, ok := obj["model"].(string); ok && m != "" {
			ru.Model = m
		}
		if ru.InputTokens > 0 || ru.OutputTokens > 0 {
			ru.BillableTokens = ru.InputTokens + ru.OutputTokens
			return ru
		}
	}

	eventType, _ := obj["type"].(string)

	// 2. Anthropic streaming: message_delta
	if eventType == "message_delta" {
		usageMap, ok := obj["usage"].(map[string]any)
		if !ok || usageMap == nil {
			return nil
		}
		ru := &RequestUsage{Timestamp: time.Now()}
		ru.OutputTokens = readInt64(usageMap, "output_tokens")
		if ru.OutputTokens == 0 {
			return nil
		}
		ru.BillableTokens = ru.OutputTokens
		return ru
	}

	// 3. Anthropic streaming: message_start
	if eventType == "message_start" {
		msg, ok := obj["message"].(map[string]any)
		if !ok || msg == nil {
			return nil
		}
		usageMap, ok := msg["usage"].(map[string]any)
		if !ok || usageMap == nil {
			return nil
		}
		ru := &RequestUsage{Timestamp: time.Now()}
		applyAnthropicInputUsage(ru, usageMap)
		if ru.InputTokens == 0 {
			return nil
		}
		if model, ok := msg["model"].(string); ok {
			ru.Model = model
		}
		return ru
	}

	// 4. Anthropic non-streaming: type == "message"
	if eventType == "message" {
		usageMap, ok := obj["usage"].(map[string]any)
		if !ok || usageMap == nil {
			return nil
		}
		ru := &RequestUsage{Timestamp: time.Now()}
		applyAnthropicInputUsage(ru, usageMap)
		ru.OutputTokens = readInt64(usageMap, "output_tokens")
		if model, ok := obj["model"].(string); ok {
			ru.Model = model
		}
		if ru.InputTokens > 0 || ru.OutputTokens > 0 {
			ru.BillableTokens = ru.InputTokens + ru.OutputTokens
			return ru
		}
	}

	return nil
}

func (p *ZAIProvider) ParseUsageHeaders(acc *Account, headers http.Header) {
	snap, ok := parseZAIResponseRateLimits(headers)
	if !ok {
		return
	}
	acc.mu.Lock()
	acc.Usage = mergeUsage(acc.Usage, snap)
	acc.mu.Unlock()
}

func parseZAIResponseRateLimits(headers http.Header) (UsageSnapshot, bool) {
	if headers == nil {
		return UsageSnapshot{}, false
	}

	snap := UsageSnapshot{
		RetrievedAt: time.Now(),
		Source:      "headers",
	}
	hasPrimary := false
	hasSecondary := false

	// Check standard request rate limit headers
	for _, key := range []string{
		"x-ratelimit-requests-utilization",
		"anthropic-ratelimit-requests-utilization",
	} {
		if pct, ok := parseRateLimitPercent(headers.Get(key)); ok {
			snap.PrimaryUsedPercent = pct
			snap.PrimaryUsed = pct
			hasPrimary = true
			break
		}
	}
	if !hasPrimary {
		for _, pair := range [][2]string{
			{"x-ratelimit-remaining-requests", "x-ratelimit-limit-requests"},
			{"x-ratelimit-requests-remaining", "x-ratelimit-requests-limit"},
			{"anthropic-ratelimit-requests-remaining", "anthropic-ratelimit-requests-limit"},
			{"x-ratelimit-remaining", "x-ratelimit-limit"},
		} {
			if pct, ok := parseRateLimitUsageFromRemainingLimit(headers, pair[0], pair[1]); ok {
				snap.PrimaryUsedPercent = pct
				snap.PrimaryUsed = pct
				hasPrimary = true
				break
			}
		}
	}

	// Check token rate limit headers
	for _, key := range []string{
		"x-ratelimit-tokens-utilization",
		"anthropic-ratelimit-tokens-utilization",
	} {
		if pct, ok := parseRateLimitPercent(headers.Get(key)); ok {
			snap.SecondaryUsedPercent = pct
			snap.SecondaryUsed = pct
			hasSecondary = true
			break
		}
	}
	if !hasSecondary {
		for _, pair := range [][2]string{
			{"x-ratelimit-remaining-tokens", "x-ratelimit-limit-tokens"},
			{"x-ratelimit-tokens-remaining", "x-ratelimit-tokens-limit"},
			{"anthropic-ratelimit-tokens-remaining", "anthropic-ratelimit-tokens-limit"},
		} {
			if pct, ok := parseRateLimitUsageFromRemainingLimit(headers, pair[0], pair[1]); ok {
				snap.SecondaryUsedPercent = pct
				snap.SecondaryUsed = pct
				hasSecondary = true
				break
			}
		}
	}

	if !hasPrimary && !hasSecondary {
		return UsageSnapshot{}, false
	}

	// These are immediate API request/token rate limits, not Coding Plan's
	// 5-hour/weekly quota. Report only fields actually supplied upstream.
	snap.PrimaryUsageReported = hasPrimary
	snap.SecondaryUsageReported = hasSecondary
	snap.primarySet = hasPrimary
	snap.secondarySet = hasSecondary

	// Check reset timestamps
	for _, key := range []string{
		"x-ratelimit-reset-requests",
		"x-ratelimit-requests-reset",
		"anthropic-ratelimit-requests-reset",
		"x-ratelimit-reset",
	} {
		if resetAt, ok := parseRateLimitReset(headers.Get(key)); ok {
			snap.PrimaryResetAt = resetAt
			break
		}
	}
	for _, key := range []string{
		"x-ratelimit-reset-tokens",
		"x-ratelimit-tokens-reset",
		"anthropic-ratelimit-tokens-reset",
	} {
		if resetAt, ok := parseRateLimitReset(headers.Get(key)); ok {
			snap.SecondaryResetAt = resetAt
			break
		}
	}

	return snap, true
}

func (p *ZAIProvider) UpstreamURL(path string) *url.URL {
	return p.zaiBase
}

func (p *ZAIProvider) MatchesPath(path string) bool {
	// Z.ai is model-routed.
	return false
}

func (p *ZAIProvider) NormalizePath(path string) string {
	return path
}

func (p *ZAIProvider) DetectsSSE(path string, contentType string) bool {
	return strings.Contains(strings.ToLower(contentType), "text/event-stream")
}

func isZAIModel(model string) bool {
	_, ok := modelForProvider(AccountTypeZAI, model)
	return ok
}

func zaiCanonicalModel(model string) string {
	if found, ok := modelForProvider(AccountTypeZAI, model); ok {
		return found.ID
	}
	return model
}
