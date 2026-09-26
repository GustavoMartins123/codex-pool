package main

import (
	"bytes"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
)

func testGenericProvider(t *testing.T) (*GenericOpenAIProvider, *Account) {
	t.Helper()
	t.Setenv("VLLM_TEST_TOKEN", "secret-vllm-token")
	provider, err := NewGenericOpenAIProvider("local-vllm", GenericProviderConfig{
		Protocol: "openai",
		BaseURL:  "https://vllm.example/v1",
		Auth:     GenericProviderAuthConfig{Type: "bearer", TokenEnv: "VLLM_TEST_TOKEN"},
		Models: []GenericProviderModelConfig{{
			ID: "qwen", Name: "Qwen Local", ContextWindow: 262144,
			MaxOutputTokens: 32768, Reasoning: true, Modalities: []string{"text"},
		}},
	})
	if err != nil {
		t.Fatal(err)
	}
	return provider, provider.syntheticAccount()
}

func TestGenericProviderRoutesNamespacedModelAndHidesSecret(t *testing.T) {
	provider, account := testGenericProvider(t)
	if canonical, ok := provider.ResolveModel("local-vllm/qwen"); !ok || canonical != "qwen" {
		t.Fatalf("ResolveModel = %q, %v", canonical, ok)
	}
	request := httptest.NewRequest(http.MethodPost, "https://pool.test/v1/chat/completions", nil)
	provider.SetAuthHeaders(request, account)
	if request.Header.Get("Authorization") != "Bearer secret-vllm-token" {
		t.Fatalf("authorization = %q", request.Header.Get("Authorization"))
	}
	descriptors := poolModelDescriptors(newPoolState([]*Account{account}, false))
	var encoded bytes.Buffer
	if err := json.NewEncoder(&encoded).Encode(descriptors); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(encoded.String(), "secret-vllm-token") {
		t.Fatal("model catalog leaked provider secret")
	}
	found := false
	for _, descriptor := range descriptors {
		if descriptor.ID == "local-vllm/qwen" {
			found = descriptor.Provider == "local-vllm" && descriptor.Protocol == "openai" && descriptor.ContextWindow == 262144
		}
	}
	if !found {
		t.Fatalf("generic descriptor missing or incorrect: %s", encoded.String())
	}
}

func TestGenericProviderParticipatesInProxyRouting(t *testing.T) {
	t.Setenv("POOL_JWT_SECRET", "generic-routing-secret")
	provider, account := testGenericProvider(t)
	base, _ := url.Parse("https://codex.example")
	var capturedURL, capturedAuth, capturedModel string
	transport := roundTripFunc(func(request *http.Request) (*http.Response, error) {
		capturedURL = request.URL.String()
		capturedAuth = request.Header.Get("Authorization")
		body, err := io.ReadAll(request.Body)
		if err != nil {
			return nil, err
		}
		var payload map[string]any
		if err := json.Unmarshal(body, &payload); err != nil {
			return nil, err
		}
		capturedModel, _ = payload["model"].(string)
		return &http.Response{
			StatusCode: http.StatusOK,
			Status:     "200 OK",
			Header:     http.Header{"Content-Type": {"text/event-stream"}},
			Body:       io.NopCloser(strings.NewReader("data: [DONE]\n\n")),
		}, nil
	})
	handler := &proxyHandler{
		cfg:       &config{maxAttempts: 1, maxInMemoryBodyBytes: 1 << 20},
		pool:      newPoolState([]*Account{account}, false),
		registry:  NewProviderRegistry(NewCodexProvider(base, base, nil), NewClaudeProvider(base), NewGeminiProvider(base, base), provider),
		transport: transport,
		metrics:   newMetrics(),
		recent:    newRecentErrors(5),
		aliases:   newModelAliases(nil),
	}
	body := `{"model":"local-vllm/qwen","stream":true,"messages":[{"role":"user","content":"hi"}]}`
	request := httptest.NewRequest(http.MethodPost, "/v1/chat/completions", strings.NewReader(body))
	request.Header.Set("Authorization", "Bearer "+generateClaudePoolToken("generic-routing-secret", "client"))
	recorder := httptest.NewRecorder()
	handler.proxyRequest(recorder, request, "generic-route")
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	if capturedURL != "https://vllm.example/v1/chat/completions" || capturedModel != "qwen" {
		t.Fatalf("url=%q model=%q", capturedURL, capturedModel)
	}
	if capturedAuth != "Bearer secret-vllm-token" {
		t.Fatalf("upstream auth=%q", capturedAuth)
	}
}

func TestGenericProviderCanJoinFallbackGraph(t *testing.T) {
	provider, account := testGenericProvider(t)
	pool := newPoolState([]*Account{account}, false)
	pool.fallbackGraph.AddCandidate("gpt-5.6-sol", "local-vllm/qwen")
	model, _, ok := pool.fallbackGraph.ResolveFallback(
		"gpt-5.6-sol", TriggerUnavailable, RequestCapabilities{},
		pool, nil,
	)
	if !ok || model != "local-vllm/qwen" {
		t.Fatalf("fallback = %q, %v", model, ok)
	}
	_ = provider
}

func TestGenericProviderUsesSmartRouter(t *testing.T) {
	provider, first := testGenericProvider(t)
	second := provider.syntheticAccount()
	second.ID = "configured-local-vllm-2"
	first.Usage.SecondaryUsed = 0.95
	second.Usage.SecondaryUsed = 0.10
	pool := newPoolState([]*Account{first, second}, false)
	selected, profile, _, _, _, _ := pool.candidateWithRoutingTrace(
		"", nil, first.Type, "", "", "local-vllm/qwen", RoutingQuotaSaver,
	)
	if selected == nil || selected.ID != second.ID || profile != string(RoutingQuotaSaver) {
		t.Fatalf("selected=%v profile=%q", selected, profile)
	}
}
