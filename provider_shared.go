package main

import (
	"context"
	"net/http"
	"net/url"
	"path/filepath"
	"strings"
	"time"
)

// apiKeyUpstream is the routing shared by providers that are selected by model
// name and call one fixed base URL with a static API key.
type apiKeyUpstream struct {
	base *url.URL
}

func (u apiKeyUpstream) UpstreamURL(string) *url.URL { return u.base }

func (apiKeyUpstream) MatchesPath(string) bool { return false }

func (apiKeyUpstream) NormalizePath(path string) string { return path }

func (apiKeyUpstream) DetectsSSE(_ string, contentType string) bool {
	return isEventStream(contentType)
}

func (apiKeyUpstream) RefreshToken(context.Context, *Account, http.RoundTripper) error {
	return nil
}

// bearerAuth sends the account API key as a bearer token.
type bearerAuth struct{}

func (bearerAuth) SetAuthHeaders(req *http.Request, acc *Account) {
	req.Header.Set("Authorization", "Bearer "+acc.AccessToken)
}

func isEventStream(contentType string) bool {
	return strings.Contains(strings.ToLower(contentType), "text/event-stream")
}

func newAPIKeyAccount(accountType AccountType, planType, name, path, apiKey string) *Account {
	return &Account{
		Type:        accountType,
		ID:          strings.TrimSuffix(name, filepath.Ext(name)),
		File:        path,
		AccessToken: apiKey,
		PlanType:    planType,
	}
}

// anthropicStreamUsage reads usage from Anthropic message_delta and
// message_start events.
func anthropicStreamUsage(obj map[string]any) *RequestUsage {
	switch eventType, _ := obj["type"].(string); eventType {
	case "message_delta":
		usageMap, ok := obj["usage"].(map[string]any)
		if !ok {
			return nil
		}
		ru := &RequestUsage{Timestamp: time.Now()}
		ru.OutputTokens = readInt64(usageMap, "output_tokens")
		if ru.OutputTokens == 0 {
			return nil
		}
		ru.BillableTokens = ru.OutputTokens
		return ru
	case "message_start":
		msg, ok := obj["message"].(map[string]any)
		if !ok {
			return nil
		}
		usageMap, ok := msg["usage"].(map[string]any)
		if !ok {
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
	return nil
}

// anthropicCombinedUsage reads usage from Anthropic-compatible upstreams that
// report input and output tokens together, at the top level or in
// message_start.
func anthropicCombinedUsage(obj map[string]any) *RequestUsage {
	if usageMap, ok := obj["usage"].(map[string]any); ok {
		ru := anthropicCombinedUsageFromMap(usageMap)
		if ru == nil {
			return nil
		}
		if model, ok := obj["model"].(string); ok {
			ru.Model = model
		}
		return ru
	}
	if eventType, _ := obj["type"].(string); eventType != "message_start" {
		return nil
	}
	msg, ok := obj["message"].(map[string]any)
	if !ok {
		return nil
	}
	usageMap, ok := msg["usage"].(map[string]any)
	if !ok {
		return nil
	}
	ru := anthropicCombinedUsageFromMap(usageMap)
	if ru == nil {
		return nil
	}
	if model, ok := msg["model"].(string); ok {
		ru.Model = model
	}
	return ru
}

func anthropicCombinedUsageFromMap(usageMap map[string]any) *RequestUsage {
	ru := &RequestUsage{Timestamp: time.Now()}
	applyAnthropicInputUsage(ru, usageMap)
	ru.OutputTokens = readInt64(usageMap, "output_tokens")
	if ru.InputTokens == 0 && ru.OutputTokens == 0 {
		return nil
	}
	ru.BillableTokens += ru.OutputTokens
	return ru
}
