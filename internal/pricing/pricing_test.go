package pricing

import (
	"math"
	"testing"
	"time"

	"codex-pool-proxy/internal/account"
	"codex-pool-proxy/internal/usage"
)

func testPricingData(t *testing.T) *PricingData {
	t.Helper()
	pd, err := New()
	if err != nil {
		t.Fatal(err)
	}
	return pd
}

func testRequestCost(t *testing.T, pd *PricingData, ru usage.RequestUsage) float64 {
	t.Helper()
	cost, err := pd.CalculateCost(ru)
	if err != nil {
		t.Fatal(err)
	}
	return cost
}

func TestPricingRequiresCanonicalData(t *testing.T) {
	pd := testPricingData(t)
	for _, model := range []string{"", "gpt-5.5-20991101"} {
		if _, err := pd.CalculateCost(usage.RequestUsage{Model: model}); err == nil {
			t.Fatalf("pricing %q succeeded without a catalog entry", model)
		}
	}
	if err := pd.LoadFromJSON([]byte("{}")); err == nil {
		t.Fatal("empty catalog was accepted")
	}
}

func TestCodexProLiteSubscriptionCost(t *testing.T) {
	t.Parallel()

	for _, plan := range []string{"prolite", "PROLITE", " ProLite "} {
		monthly, label := SubscriptionCost(account.AccountTypeCodex, plan)
		if monthly != 100 || label != "Codex Pro Lite" {
			t.Fatalf("SubscriptionCost(codex, %q) = (%v, %q), want (100, %q)", plan, monthly, label, "Codex Pro Lite")
		}
	}
}

func TestUnknownTokenPlanTierDoesNotInventSubscriptionSpend(t *testing.T) {
	t.Parallel()

	for _, test := range []struct {
		accountType account.AccountType
		plan        string
		label       string
	}{
		{account.AccountTypeKimi, "kimi", "Kimi Token Plan"},
		{account.AccountTypeMinimax, "minimax", "MiniMax Token Plan"},
	} {
		monthly, label := SubscriptionCost(test.accountType, test.plan)
		if monthly != 0 || label != test.label {
			t.Fatalf("SubscriptionCost(%s, %q) = (%v, %q), want (0, %q)", test.accountType, test.plan, monthly, label, test.label)
		}
	}
}

func TestEstimateSubscriptionSpendUsesObservedBillingCycles(t *testing.T) {
	now := time.Date(2026, time.July, 13, 0, 0, 0, 0, time.UTC)
	tests := []struct {
		name       string
		firstSeen  time.Time
		wantSpend  float64
		wantCycles int
	}{
		{name: "current cycle", firstSeen: now, wantSpend: 200, wantCycles: 1},
		{name: "before first renewal", firstSeen: now.Add(-29 * 24 * time.Hour), wantSpend: 200, wantCycles: 1},
		{name: "first renewal", firstSeen: now.Add(-30 * 24 * time.Hour), wantSpend: 400, wantCycles: 2},
		{name: "six observed cycles", firstSeen: now.Add(-171 * 24 * time.Hour), wantSpend: 1200, wantCycles: 6},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			spend, cycles := EstimateSubscriptionSpend(200, tt.firstSeen, now)
			if spend != tt.wantSpend || cycles != tt.wantCycles {
				t.Fatalf("EstimateSubscriptionSpend() = (%v, %d), want (%v, %d)", spend, cycles, tt.wantSpend, tt.wantCycles)
			}
		})
	}
}

func TestClaudeSonnet5PricingDoesNotIncreaseAfterLaunch(t *testing.T) {
	t.Parallel()

	for _, date := range []time.Time{
		time.Date(2026, time.August, 31, 23, 59, 59, 0, time.UTC),
		time.Date(2026, time.September, 2, 0, 0, 0, 0, time.UTC),
	} {
		got := PublishedModelPricing(date)["claude-sonnet-5"]
		if math.Abs(got.InputCostPerToken-2e-6) > 1e-12 ||
			math.Abs(got.OutputCostPerToken-10e-6) > 1e-12 ||
			math.Abs(got.CacheReadCost-0.2e-6) > 1e-12 ||
			math.Abs(got.CacheWriteCost-2.5e-6) > 1e-12 {
			t.Fatalf("Claude Sonnet 5 pricing at %s = %#v", date, got)
		}
	}
}

func TestLookupPricingUsesClaudeSonnet5Aliases(t *testing.T) {
	t.Parallel()

	pd := testPricingData(t)
	want, ok := pd.LookupPricing("claude-sonnet-5")
	if !ok {
		t.Fatal("missing Claude Sonnet 5 pricing")
	}
	for _, model := range []string{"claude-sonnet-5 [1m]", "claude-sonnet-5[1m]"} {
		got, ok := pd.LookupPricing(model)
		if !ok || got != want {
			t.Fatalf("LookupPricing(%q) = %#v, %v; want %#v, true", model, got, ok, want)
		}
	}
}

func TestLookupPricingIncludesClaudeFable51Fallback(t *testing.T) {
	t.Parallel()

	pd := testPricingData(t)
	got, ok := pd.LookupPricing("claude-fable-5-1")
	if !ok {
		t.Fatal("missing Claude Fable 5.1 fallback pricing")
	}
	if got.InputCostPerToken != 10e-6 || got.OutputCostPerToken != 50e-6 || got.CacheReadCost != 0.25e-6 {
		t.Fatalf("Claude Fable 5.1 pricing = %#v", got)
	}
}

func TestLookupPricingIncludesClaudeOpus5Fallback(t *testing.T) {
	t.Parallel()

	pd := testPricingData(t)
	got, ok := pd.LookupPricing("claude-opus-5")
	if !ok {
		t.Fatal("missing Claude Opus 5 fallback pricing")
	}
	if got.InputCostPerToken != 5e-6 || got.OutputCostPerToken != 25e-6 || got.CacheReadCost != 0.5e-6 {
		t.Fatalf("Claude Opus 5 pricing = %#v", got)
	}
	for _, model := range []string{"claude-opus-5 [1m]", "claude-opus-5[1m]"} {
		if aliased, ok := pd.LookupPricing(model); !ok || aliased != got {
			t.Fatalf("LookupPricing(%q) = %#v, %v; want %#v, true", model, aliased, ok, got)
		}
	}
}

func TestLookupPricingGLM53UsesPublishedRates(t *testing.T) {
	t.Parallel()

	pd := testPricingData(t)
	want := ModelPricing{
		InputCostPerToken:  1.4e-6,
		OutputCostPerToken: 4.4e-6,
		CacheReadCost:      0.26e-6,
	}
	// "zai.glm-5.3" must not fall through the prefix search to "zai.glm-5",
	// which is priced at the cheaper GLM-5 rates.
	for _, model := range []string{"glm-5.3", "zai.glm-5.3", "glm-5.2", "zai.glm-5.2"} {
		got, ok := pd.LookupPricing(model)
		if !ok {
			t.Fatalf("missing pricing for %q", model)
		}
		if got != want {
			t.Fatalf("LookupPricing(%q) = %#v, want %#v", model, got, want)
		}
	}

	wantFlash := ModelPricing{
		InputCostPerToken:  0.14e-6,
		OutputCostPerToken: 0.44e-6,
		CacheReadCost:      0.026e-6,
	}
	for _, model := range []string{"glm-5.3-flash", "zai.glm-5.3-flash"} {
		got, ok := pd.LookupPricing(model)
		if !ok {
			t.Fatalf("missing pricing for %q", model)
		}
		if got.InputCostPerToken != wantFlash.InputCostPerToken ||
			got.OutputCostPerToken != wantFlash.OutputCostPerToken ||
			math.Abs(got.CacheReadCost-wantFlash.CacheReadCost) > 1e-12 {
			t.Fatalf("LookupPricing(%q) = %#v, want %#v", model, got, wantFlash)
		}
	}
}

func TestLoadFromJSONParsesLongContextRates(t *testing.T) {
	t.Parallel()

	pd := &PricingData{}
	pd.LoadFromJSON([]byte(`{
		"tiered-test": {
			"input_cost_per_token": 0.000001,
			"output_cost_per_token": 0.000002,
			"cache_read_input_token_cost": 0.0000001,
			"input_cost_per_token_above_200k_tokens": 0.000003,
			"output_cost_per_token_above_200k_tokens": 0.000004,
			"cache_read_input_token_cost_above_200k_tokens": 0.0000002
		}
	}`))
	got, ok := pd.LookupPricing("tiered-test")
	if !ok {
		t.Fatal("missing parsed tiered pricing")
	}
	if got.LongContextThreshold != 200000 || got.LongInputCost != 3e-6 || got.LongOutputCost != 4e-6 || got.LongCacheReadCost != 0.2e-6 {
		t.Fatalf("tiered pricing = %#v", got)
	}
}

func TestCalculateCostUsesProviderTokenSemantics(t *testing.T) {
	t.Parallel()

	pd := testPricingData(t)
	claude := testRequestCost(t, pd, usage.RequestUsage{
		AccountType:         account.AccountTypeClaude,
		Model:               "claude-sonnet-5",
		InputTokens:         100,
		CachedInputTokens:   900,
		CacheCreationTokens: 100,
		OutputTokens:        10,
		InputTokenMode:      "exclusive",
	})
	wantClaude := 100*2e-6 + 900*0.2e-6 + 100*2.5e-6 + 10*10e-6
	if claude < wantClaude-1e-12 || claude > wantClaude+1e-12 {
		t.Fatalf("Claude cost = %.12f, want %.12f", claude, wantClaude)
	}

	openAI := testRequestCost(t, pd, usage.RequestUsage{
		AccountType:       account.AccountTypeCodex,
		Model:             "gpt-5.6-sol",
		InputTokens:       1000,
		CachedInputTokens: 900,
		OutputTokens:      10,
		InputTokenMode:    "inclusive",
	})
	wantOpenAI := 100*5e-6 + 900*0.5e-6 + 10*30e-6
	if openAI < wantOpenAI-1e-12 || openAI > wantOpenAI+1e-12 {
		t.Fatalf("OpenAI cost = %.12f, want %.12f", openAI, wantOpenAI)
	}
}

func TestCalculateCostAppliesLongContextTier(t *testing.T) {
	t.Parallel()

	pd := testPricingData(t)
	cost := testRequestCost(t, pd, usage.RequestUsage{
		AccountType:       account.AccountTypeCodex,
		Model:             "gpt-5.6-sol",
		InputTokens:       300_000,
		CachedInputTokens: 200_000,
		OutputTokens:      1_000,
		InputTokenMode:    "inclusive",
	})
	want := 100_000*10e-6 + 200_000*1e-6 + 1_000*45e-6
	if cost < want-1e-12 || cost > want+1e-12 {
		t.Fatalf("long-context cost = %.12f, want %.12f", cost, want)
	}
}

func TestAntigravityAliasesUseAPIEquivalentPricing(t *testing.T) {
	t.Parallel()

	pd := testPricingData(t)
	for alias, canonical := range map[string]string{
		"claude-opus-4-6-thinking":   "claude-opus-4-6",
		"gemini-3-flash":             "gemini-3-flash-preview",
		"gemini-3-flash-agent":       "gemini-3.5-flash",
		"gemini-3.1-flash-image":     "gemini-3-flash-preview",
		"gemini-3.1-flash-lite":      "gemini-3.5-flash-lite",
		"gemini-3.1-pro-low":         "gemini-3.1-pro-preview",
		"gemini-3.5-flash-extra-low": "gemini-3.5-flash",
		"gemini-3.6-flash-high":      "gemini-3.6-flash",
		"gemini-3.6-flash-tiered":    "gemini-3.6-flash",
		"gemini-3.7-flash-medium":    "gemini-3.7-flash",
		"gemini-3.7-flash-tiered":    "gemini-3.7-flash",
		"gemini-3.8-flash-low":       "gemini-3.8-flash",
		"gemini-3.8-flash-tiered":    "gemini-3.8-flash",
		"gpt-oss-120b-medium":        "openai.gpt-oss-120b-1:0",
	} {
		got, ok := pd.LookupPricing(alias)
		if !ok {
			t.Fatalf("missing pricing for %q", alias)
		}
		want, ok := pd.LookupPricing(canonical)
		if !ok || got != want {
			t.Fatalf("pricing for %q = %#v, want %q %#v", alias, got, canonical, want)
		}
	}
}

func TestLookupPricingDoesNotGuessByPrefix(t *testing.T) {
	t.Parallel()

	pd := testPricingData(t)
	if _, ok := pd.LookupPricing("zai.glm-5.99"); ok {
		t.Fatal("unknown model inherited a price by prefix")
	}
}

func TestCalculateCostZAIRequestIsNotFree(t *testing.T) {
	t.Parallel()

	pd := testPricingData(t)
	cost := testRequestCost(t, pd, usage.RequestUsage{
		AccountType:  account.AccountTypeZAI,
		Model:        "glm-5.3",
		InputTokens:  1_000_000,
		OutputTokens: 1_000_000,
	})
	if cost <= 0 {
		t.Fatalf("GLM-5.3 request cost = %v, want > 0", cost)
	}
	if want := 1.4 + 4.4; cost < want-1e-9 || cost > want+1e-9 {
		t.Fatalf("GLM-5.3 request cost = %v, want %v", cost, want)
	}
}
