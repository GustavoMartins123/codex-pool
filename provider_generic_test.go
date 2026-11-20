package main

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"codex-pool-proxy/internal/routing"
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
	testPoolProxyRequest(t, handler, recorder, request, "generic-route")
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

func TestGenericProviderUsesSmartRouter(t *testing.T) {
	provider, first := testGenericProvider(t)
	second := provider.syntheticAccount()
	second.ID = "configured-local-vllm-2"
	first.Usage.SecondaryUsed = 0.95
	second.Usage.SecondaryUsed = 0.10
	pool := newPoolState([]*Account{first, second}, false)
	decision := pool.selectRoute(routing.Request{
		Provider: string(first.Type), Model: "local-vllm/qwen", Profile: string(RoutingQuotaSaver),
	})
	if decision.Account != second || decision.Policy != string(RoutingQuotaSaver) {
		t.Fatalf("selected=%v profile=%q", decision.Account, decision.Policy)
	}
}

func TestGenericProviderUsagePollerLeavesAccountAlive(t *testing.T) {
	provider, account := testGenericProvider(t)
	h := &proxyHandler{
		cfg:      &config{usageRefresh: time.Minute},
		pool:     newPoolState([]*Account{account}, false),
		registry: NewProviderRegistry(nil, nil, nil, provider),
		transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			t.Fatalf("generic usage poller called %s", req.URL)
			return nil, nil
		}),
	}
	h.pollUpstreamUsage()
	if account.Dead || account.Penalty != 0 {
		t.Fatalf("generic account was penalized by the usage poller: dead=%v penalty=%v", account.Dead, account.Penalty)
	}
}

func TestGenericProviderHealthRestoresRetiredAccount(t *testing.T) {
	provider, account := testGenericProvider(t)
	account.Dead = true
	account.Penalty = 100
	account.HealthError = "usage endpoint returned 401"
	probed := make(chan string, 4)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	h := &proxyHandler{
		cfg:      &config{},
		pool:     newPoolState([]*Account{account}, false),
		registry: NewProviderRegistry(nil, nil, nil, provider),
		transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			select {
			case probed <- req.URL.String():
			default:
			}
			return &http.Response{
				StatusCode: http.StatusOK,
				Status:     "200 OK",
				Header:     make(http.Header),
				Body:       io.NopCloser(strings.NewReader(`{"data":[]}`)),
			}, nil
		}),
	}
	h.startGenericProviderHealthPoller(ctx)
	select {
	case got := <-probed:
		if got != "https://vllm.example/v1/models" {
			t.Fatalf("health probe url = %q", got)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("generic health poller never probed the provider")
	}
	deadline := time.Now().Add(5 * time.Second)
	for {
		account.mu.Lock()
		dead, penalty, healthError := account.Dead, account.Penalty, account.HealthError
		account.mu.Unlock()
		if !dead && penalty == 0 && healthError == "" {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("health success kept retirement state: dead=%v penalty=%v health=%q", dead, penalty, healthError)
		}
		time.Sleep(10 * time.Millisecond)
	}
}
