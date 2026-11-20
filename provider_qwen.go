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

const (
	defaultQwenBase         = "https://token-plan.ap-southeast-1.maas.aliyuncs.com/compatible-mode/v1"
	qwenClientContextWindow = 500000
)

type QwenProvider struct {
	base *url.URL
}

func NewQwenProvider(base *url.URL) *QwenProvider { return &QwenProvider{base: base} }
func (p *QwenProvider) ModelDiscoveryURL() *url.URL {
	return modelDiscoveryURL(p.UpstreamURL("/models"), "/models")
}

func (*QwenProvider) WireFormat() WireFormat { return WireOpenAI }

func (p *QwenProvider) Type() AccountType { return AccountTypeQwen }

type QwenAuthJSON struct {
	APIKey    string                 `json:"api_key"`
	Dead      bool                   `json:"dead"`
	Disabled  bool                   `json:"disabled"`
	TokenPlan *QwenTokenPlanConfig   `json:"token_plan,omitempty"`
	Credits   *QwenCreditSnapshot    `json:"token_plan_credits,omitempty"`
	Models    *providerModelSnapshot `json:"provider_model_snapshot,omitempty"`
}

func (p *QwenProvider) LoadAccount(name, path string, data []byte) (*Account, error) {
	var auth QwenAuthJSON
	if err := json.Unmarshal(data, &auth); err != nil {
		return nil, fmt.Errorf("parse %s: %w", path, err)
	}
	key := strings.TrimSpace(auth.APIKey)
	if key == "" {
		return nil, nil
	}
	account := &Account{
		Type: AccountTypeQwen, ID: strings.TrimSuffix(name, filepath.Ext(name)),
		File: path, AccessToken: key, PlanType: "token_plan",
		Dead: auth.Dead, Disabled: auth.Disabled,
		QwenTokenPlan: auth.TokenPlan,
	}
	if auth.Models == nil || len(auth.Models.Models) == 0 {
		account.HealthError = "Qwen model catalog is unavailable; model discovery is required"
	}
	now := time.Now()
	if auth.TokenPlan != nil {
		if err := validateQwenTokenPlan(auth.TokenPlan, now); err != nil {
			account.QwenCreditsError = err.Error()
		} else if auth.TokenPlan.Mode == qwenPersonalManual {
			account.PlanType = "token_plan_personal"
			account.QwenCredits = &QwenCreditSnapshot{Edition: "personal", Plan: auth.TokenPlan.Plan,
				Total: auth.TokenPlan.TotalCredits, Remaining: auth.TokenPlan.Remaining,
				Used:       auth.TokenPlan.TotalCredits - auth.TokenPlan.Remaining,
				CycleStart: auth.TokenPlan.CycleStart, CycleEnd: auth.TokenPlan.CycleEnd,
				UpdatedAt: auth.TokenPlan.UpdatedAt, Source: "manual",
			}
		} else {
			account.PlanType = "token_plan_team"
			if auth.TokenPlan.Mode == qwenPersonalCloud || auth.TokenPlan.Mode == qwenPersonalBrowser {
				account.PlanType = "token_plan_personal"
			}
			expectedSource := "alibaba-api"
			if auth.TokenPlan.Mode == qwenPersonalCloud || auth.TokenPlan.Mode == qwenPersonalBrowser {
				expectedSource = "alibaba-console"
			}
			if credits := auth.Credits; qwenCreditsCurrent(credits, now) && credits.Source == expectedSource {
				account.QwenCredits = credits
			}
		}
	}
	if account.QwenCredits != nil {
		account.Usage = qwenCreditUsage(account.QwenCredits, now)
	} else if account.QwenCreditsError == "" {
		account.QwenCreditsError = "Token Plan credit balance is unavailable"
	}
	return account, nil
}

func (p *QwenProvider) SetAuthHeaders(req *http.Request, acc *Account) {
	req.Header.Set("Authorization", "Bearer "+acc.AccessToken)
}

func (p *QwenProvider) RefreshToken(context.Context, *Account, http.RoundTripper) error {
	return nil
}

func (p *QwenProvider) ParseUsage(obj map[string]any) *RequestUsage {
	root := obj
	if response, ok := obj["response"].(map[string]any); ok {
		root = response
	}
	usage, ok := root["usage"].(map[string]any)
	if !ok {
		return nil
	}
	ru := &RequestUsage{Timestamp: time.Now(), InputTokenMode: "inclusive"}
	if _, chat := usage["prompt_tokens"]; chat {
		ru.InputTokens = readInt64(usage, "prompt_tokens")
		ru.OutputTokens = readInt64(usage, "completion_tokens")
		if details, ok := usage["prompt_tokens_details"].(map[string]any); ok {
			ru.CachedInputTokens = readInt64(details, "cached_tokens")
		}
		if details, ok := usage["completion_tokens_details"].(map[string]any); ok {
			ru.ReasoningTokens = readInt64(details, "reasoning_tokens")
		}
	} else {
		ru.InputTokens = readInt64(usage, "input_tokens")
		ru.OutputTokens = readInt64(usage, "output_tokens")
		if details, ok := usage["input_tokens_details"].(map[string]any); ok {
			ru.CachedInputTokens = readInt64(details, "cached_tokens")
		}
		if details, ok := usage["output_tokens_details"].(map[string]any); ok {
			ru.ReasoningTokens = readInt64(details, "reasoning_tokens")
		}
	}
	if ru.InputTokens == 0 && ru.OutputTokens == 0 {
		return nil
	}
	ru.Model, _ = root["model"].(string)
	ru.BillableTokens = clampNonNegative(ru.InputTokens - ru.CachedInputTokens + ru.OutputTokens)
	return ru
}

func (p *QwenProvider) UpstreamURL(string) *url.URL { return p.base }
func (p *QwenProvider) MatchesPath(string) bool     { return false }

func (p *QwenProvider) NormalizePath(path string) string {
	if isCodexResponsesPath(path) {
		return strings.TrimPrefix(strings.TrimPrefix(path, "/backend-api/codex"), "/v1")
	}
	return strings.TrimPrefix(path, "/v1")
}

func (p *QwenProvider) DetectsSSE(_ string, contentType string) bool {
	return strings.Contains(strings.ToLower(contentType), "text/event-stream")
}

func isQwenModel(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "qwen")
}

func prepareQwenChatRequest(body []byte) ([]byte, error) {
	var request map[string]any
	if err := json.Unmarshal(body, &request); err != nil {
		return nil, fmt.Errorf("invalid Qwen chat request: %w", err)
	}
	stream, _ := request["stream"].(bool)
	if !stream {
		return body, nil
	}
	options := map[string]any{}
	if value, exists := request["stream_options"]; exists && value != nil {
		var ok bool
		options, ok = value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("Qwen stream_options must be an object")
		}
	}
	options["include_usage"] = true
	request["stream_options"] = options
	return json.Marshal(request)
}

func qwenDiscoveredModels(models map[string]DiscoveredModel) (map[string]DiscoveredModel, error) {
	filtered := make(map[string]DiscoveredModel)
	for id, model := range models {
		if !isQwenModel(id) {
			continue
		}
		// These families use image, audio or embedding APIs, outside the text routes.
		lower := strings.ToLower(id)
		unsupported := false
		for _, family := range []string{"image", "audio", "tts", "asr", "realtime", "embedding", "rerank"} {
			if strings.Contains(lower, family) {
				unsupported = true
				break
			}
		}
		if unsupported {
			continue
		}
		model.Protocol = "openai"
		model.ReasoningEfforts, model.DefaultReasoningEffort = qwenReasoningEfforts(id)
		filtered[id] = model
	}
	if len(filtered) == 0 {
		return nil, fmt.Errorf("model endpoint returned no Qwen text models")
	}
	return filtered, nil
}

// Model Studio controls Qwen thinking with enable_thinking and caps it with
// thinking_budget (reasoning tokens), which it documents for the Qwen3 family
// (Qwen3 through Qwen3.8). Codex efforts map onto those for Qwen3 models: none
// turns thinking off, low and medium cap it, and high keeps the model default
// (thinking on, no cap). Other Qwen models expose no effort levels.
var qwenThinkingBudgets = map[string]int{"low": 4096, "medium": 16384}

// qwenThinkingOnlyModels cannot disable thinking; enable_thinking=false fails.
var qwenThinkingOnlyModels = []string{"2.4t", "qwen3.7-max-preview", "qwen3.7-max-2026-05-17"}

func qwenSupportsThinkingBudget(model string) bool {
	return strings.HasPrefix(strings.ToLower(strings.TrimSpace(model)), "qwen3")
}

func qwenReasoningEfforts(id string) ([]string, string) {
	if !qwenSupportsThinkingBudget(id) {
		return nil, ""
	}
	lower := strings.ToLower(id)
	for _, marker := range qwenThinkingOnlyModels {
		if strings.Contains(lower, marker) {
			return []string{"low", "medium", "high"}, "high"
		}
	}
	return []string{"none", "low", "medium", "high"}, "high"
}

func applyQwenReasoningEffort(model, effort string, out map[string]any) {
	switch effort {
	case "":
	case "none", "minimal":
		out["enable_thinking"] = false
	default:
		out["enable_thinking"] = true
		if budget, ok := qwenThinkingBudgets[effort]; ok && qwenSupportsThinkingBudget(model) {
			out["thinking_budget"] = budget
		}
	}
}
