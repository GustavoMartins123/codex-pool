package main

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"testing"
	"time"

	"codex-pool-proxy/internal/jsonvalue"
)

const aistudioModelsFixture = `{"models":[
	{"name":"models/gemini-3.8-flash","displayName":"Gemini 3.8 Flash","inputTokenLimit":1048576,"outputTokenLimit":65536,"supportedGenerationMethods":["generateContent","countTokens"],"thinking":true},
	{"name":"models/gemini-embedding-002","supportedGenerationMethods":["embedContent"]},
	{"name":"models/gemini-3.8-flash-tts","supportedGenerationMethods":["generateContent"]},
	{"name":"models/imagen-5","supportedGenerationMethods":["predict"]},
	{"name":"models/gemini-3.1-flash-image","supportedGenerationMethods":["generateContent"]},
	{"name":"models/nano-banana-pro-preview","supportedGenerationMethods":["generateContent"]},
	{"name":"models/deep-research-preview-04-2026","supportedGenerationMethods":["generateContent"]},
	{"name":"models/gemini-2.5-computer-use-preview-10-2025","supportedGenerationMethods":["generateContent"]},
	{"name":"models/gemini-robotics-er-2-preview","supportedGenerationMethods":["generateContent"]},
	{"name":"models/gemini-3.5-transcribe","supportedGenerationMethods":["generateContent"]}
]}`

func TestParseAIStudioModelsKeepsTextGenerationModels(t *testing.T) {
	models, err := parseAIStudioModels([]byte(aistudioModelsFixture))
	if err != nil {
		t.Fatal(err)
	}
	model, ok := models["aistudio/gemini-3.8-flash"]
	if len(models) != 1 || !ok {
		t.Fatalf("models = %v", models)
	}
	if model.ID != "aistudio/gemini-3.8-flash" || model.DisplayName != "Gemini 3.8 Flash · AI Studio" || model.ContextWindow != 1048576 || model.MaxOutputTokens != 65536 || !model.Reasoning {
		t.Fatalf("model = %+v", model)
	}
	shared, err := parseAIStudioModels([]byte(`{"models":[
		{"name":"models/antigravity-preview-05-2026","displayName":"Antigravity Agent Preview","supportedGenerationMethods":["generateContent"]},
		{"name":"models/antigravity-preview-09-2026","displayName":"Antigravity Agent Preview","supportedGenerationMethods":["generateContent"]},
		{"name":"models/gemini-4-hydrogen","supportedGenerationMethods":["generateContent"]}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{
		"aistudio/antigravity-preview-05-2026": "antigravity-preview-05-2026 · AI Studio",
		"aistudio/antigravity-preview-09-2026": "antigravity-preview-09-2026 · AI Studio",
		"aistudio/gemini-4-hydrogen":           "gemini-4-hydrogen · AI Studio",
	} {
		if shared[id].DisplayName != want {
			t.Errorf("%s display name = %q, want %q", id, shared[id].DisplayName, want)
		}
	}
	if _, err := parseAIStudioModels([]byte(`{"models":[{"name":"models/text-embedding-004","supportedGenerationMethods":["embedContent"]}]}`)); err == nil {
		t.Fatal("a catalog without text models was accepted")
	}
}

func TestAIStudioUpstreamModel(t *testing.T) {
	for input, want := range map[string]string{
		"aistudio/gemini-3.8-flash": "gemini-3.8-flash",
		"AIStudio/gemini-3.8-flash": "gemini-3.8-flash",
		"models/gemini-3.8-flash":   "gemini-3.8-flash",
	} {
		if got := aistudioUpstreamModel(input); got != want {
			t.Errorf("aistudioUpstreamModel(%q) = %q, want %q", input, got, want)
		}
	}
	if isAIStudioModel("gemini-3.8-flash") || !isAIStudioModel("aistudio/gemini-3.8-flash") {
		t.Fatal("AI Studio routes must require the aistudio/ prefix")
	}
}

func TestAIStudioDiscoveryPublishesResponsesModels(t *testing.T) {
	dir := t.TempDir()
	file := filepath.Join(dir, "key.json")
	if err := os.WriteFile(file, []byte(`{"api_key":"AIza-test-key"}`), 0600); err != nil {
		t.Fatal(err)
	}
	provider := NewAIStudioProvider(mustParse("https://aistudio.example"))
	data, _ := os.ReadFile(file)
	account, err := provider.LoadAccount("key.json", file, data)
	if err != nil || account == nil || account.AccessToken != "AIza-test-key" || account.HealthError == "" {
		t.Fatalf("account = %+v err=%v", account, err)
	}
	account.vault = plainTestVault()
	// A stale dead flag from an earlier probe must not outlive a valid key.
	account.Dead, account.Penalty = true, 100
	status := http.StatusOK
	transport := roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if req.URL.String() != "https://aistudio.example/v1beta/models?pageSize=1000" || req.Header.Get("x-goog-api-key") != "AIza-test-key" || req.Header.Get("Authorization") != "" {
			t.Fatalf("discovery request = %s headers=%v", req.URL, req.Header)
		}
		return qwenJSONResponse(status, aistudioModelsFixture), nil
	})
	registry := NewProviderRegistry(nil, nil, nil, provider)
	if err := syncProviderModels(context.Background(), transport, registry, account); err != nil {
		t.Fatal(err)
	}
	if account.HealthError != "" || len(account.Models) != 1 || account.Dead || account.Penalty != 0 {
		t.Fatalf("account after discovery: health=%q dead=%v penalty=%v models=%v", account.HealthError, account.Dead, account.Penalty, account.Models)
	}
	saved, _ := os.ReadFile(file)
	reloaded, err := provider.LoadAccount("key.json", file, saved)
	if err != nil || reloaded.HealthError != "" {
		t.Fatalf("persisted model snapshot was not reloaded: %+v %v", reloaded, err)
	}

	descriptors := discoveredModelsForPool(newPoolState([]*Account{account}, false))
	if len(descriptors) != 1 || descriptors[0].ID != "aistudio/gemini-3.8-flash" || !supportsAgent(descriptors[0].Protocols, "responses") || !supportsAgent(descriptors[0].Protocols, "anthropic") {
		t.Fatalf("pool descriptors = %+v", descriptors)
	}

	status = http.StatusBadRequest
	if err := syncProviderModels(context.Background(), transport, registry, account); err == nil {
		t.Fatal("a rejected API key did not fail discovery")
	}
	if account.HealthError == "" || len(account.Models) != 0 {
		t.Fatalf("failed discovery kept routing state: health=%q models=%v", account.HealthError, account.Models)
	}
}

func TestAIStudioUsagePollerLeavesKeysAlive(t *testing.T) {
	file := filepath.Join(t.TempDir(), "key.json")
	if err := os.WriteFile(file, []byte(`{"api_key":"AIza-poller"}`), 0600); err != nil {
		t.Fatal(err)
	}
	account := aistudioTestAccount("key", "AIza-poller")
	account.vault = plainTestVault()
	account.File = file
	h := &proxyHandler{
		cfg:  &config{usageRefresh: time.Minute},
		pool: newPoolState([]*Account{account}, false),
		transport: roundTripFunc(func(req *http.Request) (*http.Response, error) {
			t.Fatalf("AI Studio usage poller called %s", req.URL)
			return nil, nil
		}),
	}
	h.pollUpstreamUsage()
	if account.Dead {
		t.Fatal("AI Studio key was retired by the usage poller")
	}
}

func TestAIStudioCatalogPublishesDocumentedThinkingLevels(t *testing.T) {
	models, err := parseAIStudioModels([]byte(`{"models":[
		{"name":"models/gemini-4-hydrogen","supportedGenerationMethods":["generateContent"]},
		{"name":"models/gemini-2.5-flash","supportedGenerationMethods":["generateContent"]},
		{"name":"models/gemma-4-31b-it","supportedGenerationMethods":["generateContent"]}
	]}`))
	if err != nil {
		t.Fatal(err)
	}
	for id, want := range map[string]string{
		"aistudio/gemini-4-hydrogen": "[low medium high xhigh]/high",
		"aistudio/gemini-2.5-flash":  "[none low medium high]/high",
		"aistudio/gemma-4-31b-it":    "[]/",
	} {
		model := models[id]
		if got := fmt.Sprintf("%v/%s", model.ReasoningEfforts, model.DefaultReasoningEffort); got != want {
			t.Errorf("%s levels = %s, want %s", id, got, want)
		}
	}
}

func TestAIStudioMapsEffortToDocumentedThinking(t *testing.T) {
	for _, test := range []struct {
		name, path, body string
		want             string // thinkingConfig JSON, or "" when absent
	}{
		{"gemini 4 xhigh", "/v1/responses", `{"model":"aistudio/gemini-4-hydrogen","input":"hi","reasoning":{"effort":"xhigh"}}`, `{"includeThoughts":true,"thinkingLevel":"xhigh"}`},
		{"undocumented level keeps default", "/v1/responses", `{"model":"aistudio/gemini-3.8-flash","input":"hi","reasoning":{"effort":"xhigh"}}`, `{"includeThoughts":true}`},
		{"gemini 3 cannot turn off", "/v1/responses", `{"model":"aistudio/gemini-3.8-flash","input":"hi","reasoning":{"effort":"none"}}`, `{"includeThoughts":false}`},
		{"gemini 2.5 budget", "/v1/responses", `{"model":"aistudio/gemini-2.5-flash","input":"hi","reasoning":{"effort":"low"}}`, `{"includeThoughts":true,"thinkingBudget":1024}`},
		{"gemini 2.5 off", "/v1/chat/completions", `{"model":"aistudio/gemini-2.5-flash","messages":[{"role":"user","content":"hi"}],"reasoning_effort":"none"}`, `{"includeThoughts":false,"thinkingBudget":0}`},
		{"2.5 pro cannot turn off", "/v1/responses", `{"model":"aistudio/gemini-2.5-pro","input":"hi","reasoning":{"effort":"none"}}`, `{"includeThoughts":false}`},
		{"claude effort", "/v1/messages", `{"model":"aistudio/gemini-4-argon","max_tokens":64,"messages":[{"role":"user","content":"hi"}],"output_config":{"effort":"medium"}}`, `{"includeThoughts":true,"thinkingLevel":"medium"}`},
		{"undocumented model", "/v1/responses", `{"model":"aistudio/gemma-4-31b-it","input":"hi","reasoning":{"effort":"high"}}`, ``},
	} {
		t.Run(test.name, func(t *testing.T) {
			prepared, err := prepareAIStudioRequest(test.path, []byte(test.body), jsonvalue.String(func() map[string]any {
				var root map[string]any
				_ = json.Unmarshal([]byte(test.body), &root)
				return root
			}()["model"]), "")
			if err != nil {
				t.Fatal(err)
			}
			wire, err := aistudioWireBody(prepared)
			if err != nil {
				t.Fatal(err)
			}
			var request map[string]any
			if err := json.Unmarshal(wire, &request); err != nil {
				t.Fatal(err)
			}
			got := ""
			if config := jsonvalue.Map(jsonvalue.Map(request["generationConfig"])["thinkingConfig"]); config != nil {
				encoded, _ := json.Marshal(config)
				got = string(encoded)
			}
			if got != test.want {
				t.Fatalf("thinkingConfig = %s, want %s", got, test.want)
			}
		})
	}
}
