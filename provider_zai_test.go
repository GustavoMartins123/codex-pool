package main

import (
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"
)

func TestIsZAIModelHandlesCodingPlanModels(t *testing.T) {
	t.Parallel()

	for model, wantCanonical := range map[string]string{
		"glm-5.3":       "glm-5.3",
		"GLM-5.3":       "glm-5.3",
		"glm-5.3-flash": "glm-5.3-flash",
		"GLM-5.3-Flash": "glm-5.3-flash",
		"glm-5.2":       "glm-5.3", // Upgrade existing installed configurations.
		"GLM-5.2":       "glm-5.3",
	} {
		if !isZAIModel(model) {
			t.Fatalf("expected %q to route to zai", model)
		}
		if got := zaiCanonicalModel(model); got != wantCanonical {
			t.Fatalf("canonical model for %q = %q, want %q", model, got, wantCanonical)
		}
	}

	for _, model := range []string{"glm-4.5", "glm-4.5-air", "glm-4.6", "glm-4.7", "glm-5", "glm-5-turbo", "glm-5.1"} {
		if isZAIModel(model) {
			t.Fatalf("did not expect %q to route to zai", model)
		}
	}
}

func TestModelRouteOverrideZAIModelUsesZAIBase(t *testing.T) {
	t.Parallel()

	zaiBase, _ := url.Parse("https://api.z.ai/api/anthropic")
	handler := &proxyHandler{
		registry: NewProviderRegistry(
			&CodexProvider{},
			&ClaudeProvider{},
			&GeminiProvider{},
			NewZAIProvider(zaiBase),
		),
	}

	provider, base, rewritten := handler.modelRouteOverride("/v1/messages", "GLM-5.2", []byte(`{"model":"GLM-5.2"}`))
	if provider == nil {
		t.Fatal("expected override provider")
	}
	if provider.Type() != AccountTypeZAI {
		t.Fatalf("expected zai provider, got %s", provider.Type())
	}
	if base == nil || base.String() != zaiBase.String() {
		t.Fatalf("expected zai base %s, got %v", zaiBase, base)
	}
	if string(rewritten) != `{"model":"glm-5.3"}` {
		t.Fatalf("unexpected rewritten body: %s", rewritten)
	}
}

func TestZAIAccountLoadAndPlanSupport(t *testing.T) {
	data := []byte(`{
		"api_key": "test.api.key",
		"plan_type": "coding_plan",
		"label": "Custom Z.ai",
		"monthly_cost": 25.0,
		"window_minutes": 1440,
		"limit_tpm": 50000
	}`)

	provider := NewZAIProvider(mustParse("https://api.z.ai/api/anthropic"))
	acc, err := provider.LoadAccount("zai_test.json", "/fake/path/zai_test.json", data)
	if err != nil {
		t.Fatalf("LoadAccount failed: %v", err)
	}
	if acc.PlanType != "coding_plan" {
		t.Errorf("PlanType = %q, want coding_plan", acc.PlanType)
	}
	if acc.Label != "Custom Z.ai" {
		t.Errorf("Label = %q, want Custom Z.ai", acc.Label)
	}
	if acc.MonthlyCost != 25.0 {
		t.Errorf("MonthlyCost = %v, want 25.0", acc.MonthlyCost)
	}
	if acc.DailyTokenLimit != 50000*1440 {
		t.Errorf("DailyTokenLimit = %v, want %v", acc.DailyTokenLimit, 50000*1440)
	}
	if acc.Usage.PrimaryWindowMinutes != 1440 {
		t.Errorf("PrimaryWindowMinutes = %v, want 1440", acc.Usage.PrimaryWindowMinutes)
	}
	if acc.Usage.SecondaryWindowMinutes != 10080 {
		t.Errorf("SecondaryWindowMinutes = %v, want 10080", acc.Usage.SecondaryWindowMinutes)
	}
	if acc.Usage.PrimaryResetAt.IsZero() {
		t.Errorf("PrimaryResetAt should not be zero")
	}
	if acc.Usage.SecondaryResetAt.IsZero() {
		t.Errorf("SecondaryResetAt should not be zero")
	}
}

func TestZAIParseUsageOpenAIAndAnthropicFormats(t *testing.T) {
	provider := NewZAIProvider(mustParse("https://api.z.ai/api/anthropic"))

	// 1. Anthropic Non-streaming (type: "message")
	anthropicNonStream := map[string]any{
		"id":    "msg_123",
		"type":  "message",
		"model": "glm-5.3",
		"usage": map[string]any{
			"input_tokens":              float64(100),
			"output_tokens":             float64(50),
			"cache_read_input_tokens":   float64(20),
			"service_tier":              "standard",
		},
	}
	u1 := provider.ParseUsage(anthropicNonStream)
	if u1 == nil {
		t.Fatal("expected usage for Anthropic non-streaming message")
	}
	if u1.InputTokens != 100 || u1.OutputTokens != 50 || u1.CachedInputTokens != 20 || u1.BillableTokens != 150 {
		t.Errorf("Anthropic non-stream usage mismatch: %+v", u1)
	}

	// 2. OpenAI format
	openAIResp := map[string]any{
		"id":    "chatcmpl_123",
		"model": "glm-5.3",
		"usage": map[string]any{
			"prompt_tokens":     float64(200),
			"completion_tokens": float64(80),
			"total_tokens":      float64(280),
			"prompt_tokens_details": map[string]any{
				"cached_tokens": float64(40),
			},
			"completion_tokens_details": map[string]any{
				"reasoning_tokens": float64(25),
			},
		},
	}
	u2 := provider.ParseUsage(openAIResp)
	if u2 == nil {
		t.Fatal("expected usage for OpenAI response")
	}
	if u2.InputTokens != 200 || u2.OutputTokens != 80 || u2.CachedInputTokens != 40 || u2.ReasoningTokens != 25 || u2.BillableTokens != 280 {
		t.Errorf("OpenAI usage mismatch: %+v", u2)
	}

	// 3. Anthropic Streaming (message_start + message_delta)
	msgStart := map[string]any{
		"type": "message_start",
		"message": map[string]any{
			"model": "glm-5.3",
			"usage": map[string]any{
				"input_tokens":            float64(150),
				"cache_read_input_tokens": float64(30),
			},
		},
	}
	uStart := provider.ParseUsage(msgStart)
	if uStart == nil || uStart.InputTokens != 150 || uStart.CachedInputTokens != 30 {
		t.Errorf("message_start usage mismatch: %+v", uStart)
	}

	msgDelta := map[string]any{
		"type": "message_delta",
		"usage": map[string]any{
			"output_tokens": float64(45),
		},
	}
	uDelta := provider.ParseUsage(msgDelta)
	if uDelta == nil || uDelta.OutputTokens != 45 || uDelta.BillableTokens != 45 {
		t.Errorf("message_delta usage mismatch: %+v", uDelta)
	}
}

func TestZAIParseUsageHeaders(t *testing.T) {
	provider := NewZAIProvider(mustParse("https://api.z.ai/api/anthropic"))

	acc := &Account{
		Type: AccountTypeZAI,
		ID:   "zai_test",
	}

	hdrs := make(http.Header)
	hdrs.Set("x-ratelimit-remaining-requests", "45")
	hdrs.Set("x-ratelimit-limit-requests", "60")
	hdrs.Set("x-ratelimit-remaining-tokens", "400000")
	hdrs.Set("x-ratelimit-limit-tokens", "500000")
	hdrs.Set("x-ratelimit-reset-requests", "2026-09-22T18:00:00Z")

	provider.ParseUsageHeaders(acc, hdrs)

	// Requests: 1.0 - 45/60 = 0.25 (25% used)
	if acc.Usage.PrimaryUsedPercent < 0.24 || acc.Usage.PrimaryUsedPercent > 0.26 {
		t.Errorf("PrimaryUsedPercent = %v, want 0.25", acc.Usage.PrimaryUsedPercent)
	}
	// Tokens: 1.0 - 400k/500k = 0.20 (20% used)
	if acc.Usage.SecondaryUsedPercent < 0.19 || acc.Usage.SecondaryUsedPercent > 0.21 {
		t.Errorf("SecondaryUsedPercent = %v, want 0.20", acc.Usage.SecondaryUsedPercent)
	}
	if acc.Usage.PrimaryResetAt.IsZero() {
		t.Errorf("PrimaryResetAt should be set from headers")
	}
}

func TestSyncZAIUsageMockServer(t *testing.T) {
	mockServer := &mockUpstreamTransport{
		handler: http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
			if strings.HasSuffix(r.URL.Path, "/v1/models") {
				w.Header().Set("x-ratelimit-remaining-requests", "55")
				w.Header().Set("x-ratelimit-limit-requests", "60")
				w.Header().Set("x-ratelimit-remaining-tokens", "900000")
				w.Header().Set("x-ratelimit-limit-tokens", "1000000")
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"data":[{"id":"glm-5.3","display_name":"GLM-5.3"},{"id":"glm-5.3-flash","display_name":"GLM-5.3-Flash"}]}`))
				return
			}
			http.NotFound(w, r)
		}),
	}

	zaiBase, _ := url.Parse("http://mock-zai.local/api/anthropic")
	h := &proxyHandler{
		cfg:       &config{zaiBase: zaiBase},
		transport: mockServer,
	}

	acc := &Account{
		Type:        AccountTypeZAI,
		ID:          "zai_mock",
		AccessToken: "test-key",
		PlanType:    "coding_plan",
	}

	now := time.Now()
	err := h.syncZAIUsage(now, acc)
	if err != nil {
		t.Fatalf("syncZAIUsage failed: %v", err)
	}

	if len(acc.Models) != 2 {
		t.Errorf("expected 2 discovered models, got %d", len(acc.Models))
	}
	if !usagePrimaryWindowAvailable(acc.Usage) {
		t.Errorf("expected PrimaryWindowAvailable to be true")
	}
	if !usageSecondaryWindowAvailable(acc.Usage) {
		t.Errorf("expected SecondaryWindowAvailable to be true")
	}
	if acc.Usage.PrimaryUsedPercent < 0.08 || acc.Usage.PrimaryUsedPercent > 0.09 {
		t.Errorf("PrimaryUsedPercent = %v, want ~0.083", acc.Usage.PrimaryUsedPercent)
	}
	if acc.Usage.SecondaryUsedPercent < 0.09 || acc.Usage.SecondaryUsedPercent > 0.11 {
		t.Errorf("SecondaryUsedPercent = %v, want 0.10", acc.Usage.SecondaryUsedPercent)
	}
}
