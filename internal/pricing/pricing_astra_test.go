package pricing

import (
	"math"
	"testing"

	"codex-pool-proxy/internal/account"
	"codex-pool-proxy/internal/usage"
)

func TestAstraCostThreshold(t *testing.T) {
	pd := testPricingData(t)
	for _, tt := range []struct {
		name  string
		input int64
		want  float64
	}{
		{"below", 271999, 161999*10e-6 + 100000*1e-6 + 10000*12.5e-6 + 1000*50e-6},
		{"at", 272000, 162000*10e-6 + 100000*1e-6 + 10000*12.5e-6 + 1000*50e-6},
		{"above", 272001, 162001*20e-6 + 100000*2e-6 + 10000*25e-6 + 1000*75e-6},
	} {
		t.Run(tt.name, func(t *testing.T) {
			got := testRequestCost(t, pd, usage.RequestUsage{AccountType: account.AccountTypeCodex, Model: "gpt-6-astra", InputTokens: tt.input, CachedInputTokens: 100000, CacheCreationTokens: 10000, OutputTokens: 1000, ReasoningTokens: 800, InputTokenMode: "inclusive"})
			if math.Abs(got-tt.want) > 1e-12 {
				t.Fatalf("cost = %.12f, want %.12f", got, tt.want)
			}
		})
	}
}

func TestNonCodexReasoningCost(t *testing.T) {
	pd := testPricingData(t)
	for _, provider := range []account.AccountType{account.AccountTypeGemini, account.AccountTypeAntigravity, account.AccountTypeClaude} {
		ru := usage.RequestUsage{AccountType: provider, Model: "gemini-3.1-pro-preview", OutputTokens: 100, ReasoningTokens: 200}
		want := 300 * 12e-6
		if got := testRequestCost(t, pd, ru); math.Abs(got-want) > 1e-12 {
			t.Errorf("%s cost = %.12f, want %.12f", provider, got, want)
		}
	}
}
