package main

import (
	"encoding/json"
	"math"
	"testing"
	"time"

	"codex-pool-proxy/internal/pricing"
)

func TestAstraPublishedPricing(t *testing.T) {
	want := pricing.TieredPricing(10, 50, 1, 12.5, 272000, 20, 75, 2, 25)
	pd := testPricingData(t)
	for _, model := range []string{"gpt-6-astra", "gpt-6-astra[1m]", "gpt-6-astra [1m]", "gpt-6-astra-high", "gpt-6-astra[1m]-xhigh", "gpt-6-astra-none"} {
		got, ok := pd.LookupPricing(model)
		if !ok || got != want {
			t.Errorf("LookupPricing(%q) = %#v, %v; want %#v", model, got, ok, want)
		}
	}
	cost := advertisedModelCost("gpt-6-astra", time.Now())
	if cost.Input != 10 || cost.Output != 50 || cost.CacheRead != 1 || cost.CacheWrite != 12.5 {
		t.Errorf("advertised Astra cost = %#v", cost)
	}
	pd.LoadFromJSON([]byte(`{"gpt-6-astra":{"input_cost_per_token":0.000001,"output_cost_per_token":0.000002}}`))
	if got, ok := pd.LookupPricing("gpt-6-astra"); !ok || got != want {
		t.Errorf("stale fetched pricing won: %#v, %v", got, ok)
	}
	if _, ok := pd.LookupPricing("gpt-6-astra-unknown"); ok {
		t.Error("unknown Astra suffix inherited pricing")
	}
}

func TestCodexReasoningCost(t *testing.T) {
	pd := testPricingData(t)
	var event map[string]any
	// The Responses reasoning guide reports reasoning as a subset of output.
	if err := json.Unmarshal([]byte(`{"response":{"model":"gpt-5.6-sol","usage":{"input_tokens":75,"input_tokens_details":{"cached_tokens":0},"output_tokens":1186,"output_tokens_details":{"reasoning_tokens":1024},"total_tokens":1261}}}`), &event); err != nil {
		t.Fatal(err)
	}
	ru := (&CodexProvider{}).ParseUsage(event)
	if ru == nil || ru.OutputTokens != 1186 || ru.ReasoningTokens != 1024 {
		t.Fatalf("parsed usage = %#v", ru)
	}
	ru.AccountType = AccountTypeCodex
	want := 75*5e-6 + 1186*30e-6
	if got := testRequestCost(t, pd, *ru); math.Abs(got-want) > 1e-12 {
		t.Fatalf("Codex cost = %.12f, want %.12f", got, want)
	}
}

func TestAstraCacheWriteUsage(t *testing.T) {
	var event map[string]any
	if err := json.Unmarshal([]byte(`{"response":{"model":"gpt-6-astra","usage":{"input_tokens":272001,"input_tokens_details":{"cached_tokens":100000,"cache_write_tokens":10000},"output_tokens":1000,"output_tokens_details":{"reasoning_tokens":800}}}}`), &event); err != nil {
		t.Fatal(err)
	}
	ru := (&CodexProvider{}).ParseUsage(event)
	if ru == nil || ru.CacheCreationTokens != 10000 || ru.CachedInputTokens != 100000 || ru.InputTokens != 272001 {
		t.Fatalf("parsed cache usage = %#v", ru)
	}
	ru.AccountType = AccountTypeCodex
	if got, want := testRequestCost(t, testPricingData(t), *ru), 3.76502; math.Abs(got-want) > 1e-12 {
		t.Fatalf("parsed Astra cost = %.12f, want %.12f", got, want)
	}
}
