package main

import (
	"testing"
	"time"

	"codex-pool-proxy/internal/pricing"
)

func testPricingData(t *testing.T) *pricing.PricingData {
	t.Helper()
	pd, err := pricing.New()
	if err != nil {
		t.Fatal(err)
	}
	return pd
}

func testRequestCost(t *testing.T, pd *pricing.PricingData, usage RequestUsage) float64 {
	t.Helper()
	cost, err := pd.CalculateCost(usage)
	if err != nil {
		t.Fatal(err)
	}
	return cost
}

func TestAccountPlanForSubscriptionPreservesProLite(t *testing.T) {
	t.Parallel()

	for _, plan := range []string{"prolite", "PROLITE", "Codex ProLite"} {
		if got := accountPlanForSubscription(plan); got != "prolite" {
			t.Fatalf("accountPlanForSubscription(%q) = %q, want prolite", plan, got)
		}
	}
}

func TestClaude55PricingAndContextAliases(t *testing.T) {
	t.Parallel()
	pd := testPricingData(t)
	for model, want := range map[string]pricing.ModelPricing{
		"claude-opus-5-5":   pricing.FlatPricing(4, 20, 0.2, 5),
		"claude-sonnet-5-5": pricing.FlatPricing(2, 10, 0.2, 2.5),
	} {
		for _, id := range []string{model, model + "[1m]", model + " [1m]"} {
			got, ok := pd.LookupPricing(id)
			if !ok || got != want {
				t.Fatalf("LookupPricing(%q) = %#v, %v; want %#v", id, got, ok, want)
			}
			advertised := advertisedModelCost(id, time.Now())
			if advertised.Input != want.InputCostPerToken*1e6 || advertised.CacheRead != want.CacheReadCost*1e6 {
				t.Fatalf("advertised %s pricing differs from accounting: %#v", id, advertised)
			}
		}
	}
}

func TestAdvertisedPoolModelsHavePricing(t *testing.T) {
	t.Parallel()

	for _, accountType := range []AccountType{
		AccountTypeCodex, AccountTypeClaude, AccountTypeKimi, AccountTypeMinimax,
		AccountTypeZAI, AccountTypeXiaomi, AccountTypeAdverserial,
	} {
		for _, model := range modelsForProvider(accountType) {
			cost := advertisedModelCost(model.ID, time.Now())
			if cost.Input <= 0 || cost.Output <= 0 {
				t.Errorf("%s/%s advertised cost = %#v", accountType, model.ID, cost)
			}
		}
	}
	for _, model := range grokModelCatalog {
		cost := advertisedModelCost(model.ID, time.Now())
		if cost.Input <= 0 || cost.Output <= 0 {
			t.Errorf("grok/%s advertised cost = %#v", model.ID, cost)
		}
	}
}
