package main

import (
	_ "embed"
	"encoding/json"
	"errors"
	"fmt"
	"log"
	"strings"
	"sync"
	"time"

	"codex-pool-proxy/internal/account"
)

//go:embed pricing_catalog.json
var pricingCatalogJSON []byte

// ModelPricing holds per-token costs (in USD per token).
type ModelPricing struct {
	InputCostPerToken    float64 `json:"input_cost_per_token"`
	OutputCostPerToken   float64 `json:"output_cost_per_token"`
	CacheReadCost        float64 `json:"cache_read_input_token_cost"`
	CacheWriteCost       float64 `json:"cache_creation_input_token_cost"`
	LongContextThreshold int64   `json:"-"`
	LongInputCost        float64 `json:"-"`
	LongOutputCost       float64 `json:"-"`
	LongCacheReadCost    float64 `json:"-"`
	LongCacheWriteCost   float64 `json:"-"`
}

// PricingData holds the loaded pricing map and provides thread-safe lookup.
type PricingData struct {
	mu     sync.RWMutex
	models map[string]ModelPricing
}

// subscriptionCosts maps (account_type, plan_type) to monthly cost in USD.
type subscriptionKey struct {
	accountType AccountType
	planType    string
}

var subscriptionCosts = map[subscriptionKey]struct {
	monthly float64
	label   string
}{
	{AccountTypeClaude, "pro"}:                    {20, "Claude Pro"},
	{AccountTypeClaude, "max_5x"}:                 {100, "Claude Max 5x"},
	{AccountTypeClaude, "default_claude_max_5x"}:  {100, "Claude Max 5x"},
	{AccountTypeClaude, "max_20x"}:                {200, "Claude Max 20x"},
	{AccountTypeClaude, "default_claude_max_20x"}: {200, "Claude Max 20x"},
	{AccountTypeClaude, "team"}:                   {25, "Claude Team"},
	{AccountTypeCodex, "plus"}:                    {20, "Codex Plus"},
	{AccountTypeCodex, "prolite"}:                 {100, "Codex Pro Lite"},
	{AccountTypeCodex, "pro"}:                     {200, "Codex Pro"},
	{AccountTypeCodex, "team"}:                    {25, "Codex Team"},
	{AccountTypeGemini, "api"}:                    {0, "Gemini API"},
	{AccountTypeAntigravity, "antigravity"}:       {0, "Google Antigravity"},
	// The provider auth files identify Kimi and MiniMax token-plan credentials,
	// but not their paid tier. Reporting a guessed $49 or $5 monthly spend made
	// ROI look precise while being unrelated to the account's actual plan.
	{AccountTypeKimi, "kimi"}:               {0, "Kimi Token Plan"},
	{AccountTypeKimi, ""}:                   {0, "Kimi Token Plan"},
	{AccountTypeMinimax, "minimax"}:         {0, "MiniMax Token Plan"},
	{AccountTypeMinimax, ""}:                {0, "MiniMax Token Plan"},
	{AccountTypeZAI, "coding_plan"}:         {15, "Z.ai Coding Plan"},
	{AccountTypeZAI, "coding"}:              {15, "Z.ai Coding Plan"},
	{AccountTypeZAI, "pro"}:                 {30, "Z.ai Pro Plan"},
	{AccountTypeZAI, "team"}:                {50, "Z.ai Team Plan"},
	{AccountTypeZAI, "lite"}:                {5, "Z.ai Lite Plan"},
	{AccountTypeZAI, "standard"}:            {15, "Z.ai Standard Plan"},
	{AccountTypeZAI, "zai"}:                 {15, "Z.ai Coding Plan"},
	{AccountTypeZAI, ""}:                    {15, "Z.ai Coding Plan"},
	{AccountTypeXiaomi, "xiaomi"}:           {0, "Xiaomi MiMo Token Plan"},
	{AccountTypeXiaomi, ""}:                 {0, "Xiaomi MiMo Token Plan"},
	{AccountTypeAdverserial, "adverserial"}: {0, "Adverserial Platform"},
	{AccountTypeAdverserial, ""}:            {0, "Adverserial Platform"},
	{AccountTypeOpencodeGo, "opencode_go"}:  {10, "OpenCode Go"},
	{AccountTypeOpencodeGo, ""}:             {10, "OpenCode Go"},
}

// getSubscriptionCost returns monthly cost and label for an account.
func getSubscriptionCost(accType AccountType, planType string) (monthly float64, label string) {
	planType = strings.ToLower(strings.TrimSpace(planType))
	if accType == AccountTypeCodex {
		planType = account.NormalizeCodexPlanType(planType)
	}
	// Try exact match first
	if info, ok := subscriptionCosts[subscriptionKey{accType, planType}]; ok {
		return info.monthly, info.label
	}
	return 0, string(accType)
}

// estimateSubscriptionSpend counts the initial paid month plus one billing
// cycle for each completed 30-day period covered by the API-cost analytics.
func estimateSubscriptionSpend(monthly float64, firstSeen, now time.Time) (spend float64, billingCycles int) {
	if monthly <= 0 || firstSeen.IsZero() {
		return 0, 0
	}
	if now.Before(firstSeen) {
		now = firstSeen
	}
	billingCycles = int(now.Sub(firstSeen)/(30*24*time.Hour)) + 1
	return monthly * float64(billingCycles), billingCycles
}

func newPricingData() (*PricingData, error) {
	pd := &PricingData{}
	if err := pd.loadFromJSON(pricingCatalogJSON); err != nil {
		return nil, fmt.Errorf("load pricing catalog: %w", err)
	}
	return pd, nil
}

// loadFromJSON parses the LiteLLM pricing JSON format into the models map.
func (pd *PricingData) loadFromJSON(data []byte) error {
	var raw map[string]json.RawMessage
	if err := json.Unmarshal(data, &raw); err != nil {
		return err
	}

	models := make(map[string]ModelPricing, len(raw))
	for key, val := range raw {
		if key == "sample_spec" {
			continue
		}
		var entry map[string]json.RawMessage
		if err := json.Unmarshal(val, &entry); err != nil {
			continue
		}
		input, inputOK := pricingJSONFloat(entry, "input_cost_per_token")
		output, outputOK := pricingJSONFloat(entry, "output_cost_per_token")
		if !inputOK || !outputOK {
			continue
		}
		cacheRead, _ := pricingJSONFloat(entry, "cache_read_input_token_cost")
		cacheWrite, _ := pricingJSONFloat(entry, "cache_creation_input_token_cost")
		mp := ModelPricing{
			InputCostPerToken:  input,
			OutputCostPerToken: output,
			CacheReadCost:      cacheRead,
			CacheWriteCost:     cacheWrite,
		}
		for _, threshold := range []struct {
			tokens int64
			suffix string
		}{
			{128000, "128k"},
			{200000, "200k"},
			{256000, "256k"},
			{272000, "272k"},
			{512000, "512k"},
		} {
			longInput, ok := pricingJSONFloat(entry, "input_cost_per_token_above_"+threshold.suffix+"_tokens")
			if !ok {
				continue
			}
			mp.LongContextThreshold = threshold.tokens
			mp.LongInputCost = longInput
			mp.LongOutputCost = mp.OutputCostPerToken
			mp.LongCacheReadCost = mp.CacheReadCost
			mp.LongCacheWriteCost = mp.CacheWriteCost
			for key, rate := range map[string]*float64{
				"output_cost_per_token": &mp.LongOutputCost,
				"cache_read_input_token_cost": &mp.LongCacheReadCost,
				"cache_creation_input_token_cost": &mp.LongCacheWriteCost,
			} {
				if override, ok := pricingJSONFloat(entry, key+"_above_"+threshold.suffix+"_tokens"); ok {
					*rate = override
				}
			}
			break
		}
		models[key] = mp
	}
	if len(models) == 0 {
		return errors.New("pricing catalog contains no model rates")
	}
	// Fill IDs absent from LiteLLM and override the small set whose pool-facing
	// name, promotion schedule, or provider rate cannot be represented there.
	for id, pricing := range publishedModelPricing(time.Now()) {
		if _, present := models[id]; !present || forcePublishedPricing[id] {
			models[id] = pricing
		}
	}

	pd.mu.Lock()
	pd.models = models
	pd.mu.Unlock()
	log.Printf("pricing: loaded %d model prices", len(models))
	return nil
}

func pricingJSONFloat(entry map[string]json.RawMessage, key string) (float64, bool) {
	raw, ok := entry[key]
	if !ok {
		return 0, false
	}
	var value float64
	if err := json.Unmarshal(raw, &value); err != nil {
		return 0, false
	}
	return value, true
}

var pricingModelAliases = map[string]string{
	"gpt-6-astra[1m]":            "gpt-6-astra",
	"gpt-6-astra [1m]":           "gpt-6-astra",
	"gpt-6-astra-none":           "gpt-6-astra",
	"gpt-6.1-sol[1m]":            "gpt-6.1-sol",
	"gpt-6.1-sol [1m]":           "gpt-6.1-sol",
	"claude-opus-5 [1m]":         "claude-opus-5",
	"claude-opus-5[1m]":          "claude-opus-5",
	"claude-opus-5-5 [1m]":       "claude-opus-5-5",
	"claude-opus-5-5[1m]":        "claude-opus-5-5",
	"claude-sonnet-5-5 [1m]":     "claude-sonnet-5-5",
	"claude-sonnet-5-5[1m]":      "claude-sonnet-5-5",
	"claude-sonnet-5 [1m]":       "claude-sonnet-5",
	"claude-sonnet-5[1m]":        "claude-sonnet-5",
	"gpt-5.6-sol[1m]":            "gpt-5.6-sol",
	"gpt-5.6-terra[1m]":          "gpt-5.6-terra",
	"gpt-5.6-luna[1m]":           "gpt-5.6-luna",
	"gpt-daybreak-blue-latest":   "gpt-5.6-sol",
	"glm-5.2":                    "glm-5.3",
	"zai.glm-5.2":                "glm-5.3",
	"zai.glm-5.3":                "glm-5.3",
	"zai.glm-5.3-flash":          "glm-5.3-flash",
	"glm-5.3-flashx":             "glm-5.3-flash",
	"zai.glm-5.3-flashx":         "glm-5.3-flash",
	"glm-5-turbo":                "glm-5",
	"glm-4.5-air":                "glm-4.5",
	"grok-4.7-build":             "grok-4.7",
	"grok-4.6-build":             "grok-4.6",
	"grok-4.5-build":             "grok-4.5",
	"grok-build-latest":          "grok-4.6",
	"claude-opus-4-6-thinking":   "claude-opus-4-6",
	"gemini-pro-agent":           "gemini-3.1-pro-preview",
	"gemini-3-flash":             "gemini-3-flash-preview",
	"gemini-3-flash-agent":       "gemini-3.5-flash",
	"gemini-3.1-flash-image":     "gemini-3-flash-preview",
	"gemini-3.1-flash-lite":      "gemini-3.5-flash-lite",
	"gemini-3.1-pro":             "gemini-3.1-pro-preview",
	"gemini-3.5-flash-extra-low": "gemini-3.5-flash",
	"gemini-3.6-flash-tiered":    "gemini-3.6-flash",
	"gemini-3.7-flash-tiered":    "gemini-3.7-flash",
	"gemini-3.8-flash-tiered":    "gemini-3.8-flash",
	"gpt-oss-120b":               "openai.gpt-oss-120b-1:0",
	"mimo-v2.5-pro[1m]":          "mimo-v2.5-pro",
	"kimi":                       "kimi-for-coding",
	"k2p5":                       "kimi-for-coding",
	"kimi-k2-thinking":           "kimi-for-coding",
	"minimax":                    "MiniMax-M3",
	"minimax-m3":                 "MiniMax-M3",
	"cyberkimi":                  "lordx64/cyberkimi",
}

func canonicalPricingModel(model string) string {
	model = strings.TrimSpace(model)
	model = strings.TrimPrefix(model, "antigravity/")
	if alias, ok := pricingModelAliases[model]; ok {
		model = alias
	}
	for _, effort := range []string{"-minimal", "-low", "-medium", "-high", "-xhigh", "-max"} {
		if strings.HasSuffix(model, effort) {
			model = strings.TrimSuffix(model, effort)
			break
		}
	}
	if alias, ok := pricingModelAliases[model]; ok {
		model = alias
	}
	return model
}

// lookupPricing uses exact IDs and explicit aliases. It deliberately avoids
// arbitrary prefix matching: "zai.glm-5.3" previously matched "zai.glm-5"
// and silently used the wrong rate.
func (pd *PricingData) lookupPricing(model string) (ModelPricing, bool) {
	model = canonicalPricingModel(model)
	if model == "" {
		return ModelPricing{}, false
	}

	pd.mu.RLock()
	defer pd.mu.RUnlock()

	mp, ok := pd.models[model]
	return mp, ok
}

func (pd *PricingData) calculateCost(ru RequestUsage) (float64, error) {
	if strings.TrimSpace(ru.Model) == "" {
		return 0, errors.New("model is required to calculate request cost")
	}
	mp, ok := pd.lookupPricing(ru.Model)
	if !ok {
		return 0, fmt.Errorf("pricing is unavailable for model %q", ru.Model)
	}

	totalInput := ru.InputTokens
	uncachedInput := ru.InputTokens - ru.CachedInputTokens - ru.CacheCreationTokens
	if inputTokensExcludeCached(ru) {
		uncachedInput = ru.InputTokens
		totalInput += ru.CachedInputTokens + ru.CacheCreationTokens
	}
	if uncachedInput < 0 {
		uncachedInput = 0
	}

	inputCost := mp.InputCostPerToken
	outputCost := mp.OutputCostPerToken
	cacheReadCost := mp.CacheReadCost
	cacheWriteCost := mp.CacheWriteCost
	if mp.LongContextThreshold > 0 && totalInput > mp.LongContextThreshold {
		inputCost = mp.LongInputCost
		outputCost = mp.LongOutputCost
		cacheReadCost = mp.LongCacheReadCost
		cacheWriteCost = mp.LongCacheWriteCost
	}

	cost := float64(uncachedInput) * inputCost
	cost += float64(ru.CachedInputTokens) * cacheReadCost
	cost += float64(ru.CacheCreationTokens) * cacheWriteCost
	outputTokens := ru.OutputTokens
	// Codex reports reasoning inside output_tokens; Gemini reports it separately.
	if ru.AccountType != AccountTypeCodex {
		outputTokens += ru.ReasoningTokens
	}
	cost += float64(outputTokens) * outputCost
	return cost, nil
}

func inputTokensExcludeCached(ru RequestUsage) bool {
	switch ru.InputTokenMode {
	case "exclusive":
		return true
	case "inclusive":
		return false
	}
	// Historical records predate InputTokenMode. These providers expose the
	// Anthropic usage shape, where input_tokens is uncached input only.
	switch ru.AccountType {
	case AccountTypeClaude, AccountTypeKimi, AccountTypeMinimax, AccountTypeZAI, AccountTypeXiaomi, AccountTypeAdverserial, AccountTypeOpencodeGo:
		return true
	default:
		return false
	}
}
