package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"codex-pool-proxy/internal/runnerproto"
)

func TestRunnerCatalogBuildsProtocolSpecificNativeModels(t *testing.T) {
	catalog := runnerCatalog([]poolModelDescriptor{
		{ID: "pool-non-gpt", Name: "Pool model", Protocol: "responses", ReasoningEfforts: []string{"low", "high"}, DefaultReasoningEffort: "high"},
		{ID: "claude-only", Protocol: "anthropic", ReasoningEfforts: []string{"minimal", "high"}},
		{ID: "gemini-only", Protocol: "gemini"},
	})
	if err := runnerproto.ValidateAgentModels(catalog.Models); err != nil {
		t.Fatal(err)
	}
	if len(catalog.Models) != 2 || !runnerproto.SupportsModel(catalog.Models, "codex", "pool-non-gpt", "high") ||
		runnerproto.SupportsModel(catalog.Models, "codex", "claude-only", "") || runnerproto.SupportsModel(catalog.Models, "claude", "claude-only", "minimal") {
		t.Fatalf("wrong model protocols or efforts: %+v", catalog.Models)
	}
	var native struct {
		Models []struct {
			Slug    string `json:"slug"`
			Default string `json:"default_reasoning_level"`
		} `json:"models"`
	}
	if json.Unmarshal([]byte(catalog.CodexCatalog), &native) != nil || len(native.Models) != 1 || native.Models[0].Slug != "pool-non-gpt" || native.Models[0].Default != "high" {
		t.Fatalf("Codex native catalog = %s", catalog.CodexCatalog)
	}
}

func TestRunnerCatalogRequiresAnActiveRunnerCredential(t *testing.T) {
	h, principal, _, _, _, _ := memberQuotaFixture(t)
	saveQuotaPolicy(t, h, principal, ClientPolicy{Providers: PolicySelector{Allow: []string{"claude"}}})
	b, err := h.sessionBroker()
	if err != nil {
		t.Fatal(err)
	}
	credential := pairedRunner(t, b, principal)
	for _, token := range []string{"", "invalid", credential.Token} {
		r := httptest.NewRequest(http.MethodGet, "/api/runner/catalog", nil)
		if token != "" {
			r.Header.Set("Authorization", "Bearer "+token)
		}
		w := httptest.NewRecorder()
		h.ServeHTTP(w, r)
		if token != credential.Token && w.Code != http.StatusUnauthorized {
			t.Fatalf("invalid runner got %d", w.Code)
		}
		if token == credential.Token && w.Code != http.StatusOK {
			t.Fatalf("runner catalog = %d %s", w.Code, w.Body.String())
		}
		if token == credential.Token {
			var catalog runnerproto.RunnerCatalog
			if json.Unmarshal(w.Body.Bytes(), &catalog) != nil || len(catalog.Models) == 0 {
				t.Fatalf("empty authorized runner catalog: %s", w.Body.String())
			}
			for _, model := range catalog.Models {
				if model.Model != "claude-opus-5-5" {
					t.Fatalf("runner received a model outside its account policy: %+v", model)
				}
			}
		}
	}
	if err := b.revokeRunner(principal, credential.RunnerID); err != nil {
		t.Fatal(err)
	}
	r := httptest.NewRequest(http.MethodGet, "/api/runner/catalog", nil)
	r.Header.Set("Authorization", "Bearer "+credential.Token)
	w := httptest.NewRecorder()
	h.ServeHTTP(w, r)
	if w.Code != http.StatusUnauthorized {
		t.Fatalf("revoked runner got %d", w.Code)
	}
}

func TestSessionUsesOnlyTheRunnerThatConfirmedItsModel(t *testing.T) {
	_, b, login, principal := brokerFixture(t)
	first := brokerRunner(t, b, principal)
	second := brokerRunner(t, b, principal)
	first.hello.ModelsKnown = true
	first.hello.Models = []runnerproto.AgentModel{{Agent: "claude", Model: "synthetic-model", ReasoningEfforts: []string{"high"}}}
	second.hello.ModelsKnown = true
	second.hello.Models = []runnerproto.AgentModel{{Agent: "claude", Model: "another-model"}}
	for len(first.out) > 0 {
		<-first.out
	}
	for len(second.out) > 0 {
		<-second.out
	}
	q := runnerproto.CreateSession{Agent: "claude", Model: "synthetic-model", ReasoningEffort: "high", RunnerID: first.id, Workspace: runnerproto.Workspace{Mode: "ephemeral"}, PermissionMode: "gated"}
	wrong := q
	wrong.RunnerID = second.id
	if _, err := b.create(principal, login.ClientID, wrong); err == nil {
		t.Fatal("pinned an incompatible runner")
	}
	if _, err := b.create("other-owner", login.ClientID, q); err == nil {
		t.Fatal("pinned another account's runner")
	}
	s, err := b.create(principal, login.ClientID, q)
	if err != nil {
		t.Fatal(err)
	}
	if s.RunnerID != first.id || len(first.out) == 0 || len(second.out) != 0 {
		t.Fatal("session offered to the wrong runner")
	}
	if err := b.claim(second, s.ID); err == nil {
		t.Fatal("incompatible runner claimed session")
	}
	if err := b.claim(first, s.ID); err != nil {
		t.Fatal(err)
	}
	brokerEvent(t, b, first, s.ID, "workspace.status", func(m *runnerproto.Message) { m.Status = "ready" })
	model, effort := "another-model", "high"
	if _, err := b.startTurn(principal, s.ID, runnerproto.TurnInput{Input: "continue", Model: &model, ReasoningEffort: &effort}, authorizeSyntheticSession); err == nil {
		t.Fatal("unsupported model switch accepted")
	}
	model, effort = "synthetic-model", "max"
	if _, err := b.startTurn(principal, s.ID, runnerproto.TurnInput{Input: "continue", Model: &model, ReasoningEffort: &effort}, authorizeSyntheticSession); err == nil {
		t.Fatal("unsupported reasoning effort accepted")
	}
}

func TestFinalizeCodexModelCatalogAddsDiscoveredQwenModels(t *testing.T) {
	qwenModels := map[string]DiscoveredModel{
		"qwen3.6-flash":     {ID: "qwen3.6-flash", Protocol: "openai"},
		"qwen3.7-max":       {ID: "qwen3.7-max", Protocol: "openai"},
		"qwen3.7-plus":      {ID: "qwen3.7-plus", Protocol: "openai"},
		"qwen3.8-flash":     {ID: "qwen3.8-flash", Protocol: "openai"},
		"qwen3.8-max":       {ID: "qwen3.8-max", DisplayName: "Qwen 3.8 Max", Protocol: "openai", ContextWindow: 1000000},
		"qwen4-max-preview": {ID: "qwen4-max-preview", Protocol: "openai", ContextWindow: 128000},
	}
	pool := newPoolState([]*Account{
		{ID: "qwen", Type: AccountTypeQwen, AccessToken: "sk-sp-test", Models: qwenModels},
		{ID: "kimi", Type: AccountTypeKimi, AccessToken: "sk-test", Models: map[string]DiscoveredModel{
			"kimi-k3": {ID: "kimi-k3", Protocol: "anthropic"},
		}},
	}, false)
	models := discoveredModelsForPool(pool)
	for _, model := range models {
		if model.Provider == "qwen" && model.ContextWindow != 500000 {
			t.Fatalf("Qwen pool catalog context = %d for %s", model.ContextWindow, model.ID)
		}
	}
	upstream := []byte(`{"models":[{"slug":"gpt-6.1-sol","display_name":"GPT","context_window":500000,"max_context_window":872000}],"etag":"keep"}`)
	merged, err := finalizeCodexModelCatalog(upstream, models)
	if err != nil {
		t.Fatal(err)
	}
	out, err := finalizeCodexModelCatalog(merged, models)
	if err != nil {
		t.Fatal(err)
	}
	var catalog struct {
		Models []map[string]any `json:"models"`
		ETag   string           `json:"etag"`
	}
	if err := json.Unmarshal(out, &catalog); err != nil {
		t.Fatal(err)
	}
	slugs := map[string]int{}
	for _, model := range catalog.Models {
		slugs[stringValue(model["slug"])]++
	}
	if catalog.ETag != "keep" || slugs["gpt-6.1-sol"] != 1 || slugs["kimi-k3"] != 0 || len(slugs) != len(qwenModels)+1 {
		t.Fatalf("catalog slugs = %v etag=%q", slugs, catalog.ETag)
	}
	for id := range qwenModels {
		if slugs[id] != 1 {
			t.Fatalf("Qwen catalog has %d entries for %s", slugs[id], id)
		}
	}
	for _, model := range catalog.Models {
		if _, qwen := qwenModels[stringValue(model["slug"])]; qwen &&
			(model["context_window"] != float64(500000) || model["max_context_window"] != float64(500000) || model["visibility"] != "list") {
			t.Fatalf("Qwen catalog entry = %v", model)
		}
		if model["slug"] == "gpt-6.1-sol" && (model["context_window"] != float64(500000) || model["max_context_window"] != float64(872000)) {
			t.Fatalf("upstream GPT context was rewritten: %v", model)
		}
	}
	if _, err := finalizeCodexModelCatalog([]byte("not json"), models); err == nil {
		t.Fatal("invalid upstream catalog was accepted")
	}
}

func TestCodexModelsEndpointListsQwenModels(t *testing.T) {
	t.Setenv("POOL_JWT_SECRET", "codex-catalog-qwen-secret")
	codex := &Account{ID: "codex", Type: AccountTypeCodex, AccessToken: "codex-access", AccountID: "acct-codex", PlanType: "pro"}
	qwen := &Account{ID: "qwen", Type: AccountTypeQwen, AccessToken: "sk-sp-catalog", Models: map[string]DiscoveredModel{"qwen3.8-max": {ID: "qwen3.8-max", Protocol: "openai"}}}
	h := qwenTestHandler(t, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(req.URL.Path, "/codex/models") {
			t.Fatalf("unexpected upstream request %s", req.URL)
		}
		return qwenJSONResponse(200, `{"models":[{"slug":"gpt-6.1-sol","display_name":"GPT"}]}`), nil
	}), codex, qwen)
	req := httptest.NewRequest("GET", "/backend-api/codex/models?client_version=0.164.0", nil)
	req.Header.Set("Authorization", "Bearer "+generateClaudePoolToken("codex-catalog-qwen-secret", "codex-client"))
	rr := httptest.NewRecorder()
	testPoolProxyRequest(t, h, rr, req, "codex-catalog")
	if rr.Code != 200 || !strings.Contains(rr.Body.String(), `"slug":"gpt-6.1-sol"`) || !strings.Contains(rr.Body.String(), `"slug":"qwen3.8-max"`) {
		t.Fatalf("status=%d catalog=%.400s", rr.Code, rr.Body.String())
	}
	var catalog struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	for _, model := range catalog.Models {
		if model["slug"] == "qwen3.8-max" && (model["context_window"] != float64(500000) || model["max_context_window"] != float64(500000)) {
			t.Fatalf("Qwen endpoint context = %v", model)
		}
	}
}

func TestCodexModelsEndpointListsDiscoveredCodexModels(t *testing.T) {
	t.Setenv("POOL_JWT_SECRET", "codex-catalog-discovery-secret")
	upstream := &Account{ID: "upstream", Type: AccountTypeCodex, AccessToken: "codex-access", AccountID: "acct-upstream", PlanType: "pro"}
	discovered := &Account{
		ID: "discovered", Type: AccountTypeCodex, AccessToken: "codex-discovered", AccountID: "acct-discovered", PlanType: "pro",
		Models: map[string]DiscoveredModel{
			"gpt-6.2-sol": {
				ID: "gpt-6.2-sol", DisplayName: "GPT-6.2 Sol", ContextWindow: 500000,
				ReasoningEfforts: []string{"low", "medium", "high", "xhigh"}, DefaultReasoningEffort: "high",
			},
			"gpt-6.2-astra": {
				ID: "gpt-6.2-astra", DisplayName: "GPT-6.2 Astra", ContextWindow: 272000,
				ReasoningEfforts: []string{"low", "medium", "high", "xhigh", "max"}, DefaultReasoningEffort: "xhigh",
			},
		},
	}
	for id, model := range discovered.Models {
		model.CatalogEntry = upstreamCodexEntry(t, model)
		discovered.Models[id] = model
	}
	discovered.Models["gpt-6.2-terra-preview"] = DiscoveredModel{ID: "gpt-6.2-terra-preview", DisplayName: "No catalog entry"}
	grok := &Account{ID: "grok", Type: AccountTypeGrok, AccessToken: "grok-access", Models: map[string]DiscoveredModel{
		"grok-chat-only": {ID: "grok-chat-only", Protocol: "openai"},
	}}
	h := qwenTestHandler(t, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		if !strings.HasSuffix(req.URL.Path, "/codex/models") {
			t.Fatalf("unexpected upstream request %s", req.URL)
		}
		return qwenJSONResponse(200, `{"models":[
			{"slug":"upstream-other-a","priority":1},
			{"slug":"gpt-6.2-sol[1m]","priority":2},
			{"slug":"gpt-6.2-luna","priority":3},
			{"slug":"gpt-6.1-sol","display_name":"Upstream GPT","context_window":500000,"max_context_window":872000,"priority":4},
			{"slug":"gpt-6.10-luna","priority":5},
			{"slug":"gpt-6.2-terra","priority":6},
			{"slug":"upstream-other-b","priority":7}
		]}`), nil
	}), upstream, discovered, grok)
	req := httptest.NewRequest("GET", "/backend-api/codex/models?client_version=0.164.0", nil)
	req.Header.Set("Authorization", "Bearer "+generateClaudePoolToken("codex-catalog-discovery-secret", "codex-client"))
	rr := httptest.NewRecorder()
	testPoolProxyRequest(t, h, rr, req, "codex-catalog")
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d catalog=%.400s", rr.Code, rr.Body.String())
	}
	var catalog struct {
		Models []struct {
			Slug            string `json:"slug"`
			Name            string `json:"display_name"`
			ContextWindow   int    `json:"context_window"`
			MaxContext      int    `json:"max_context_window"`
			Visibility      string `json:"visibility"`
			Priority        int    `json:"priority"`
			DefaultEffort   string `json:"default_reasoning_level"`
			ReasoningLevels []struct {
				Effort string `json:"effort"`
			} `json:"supported_reasoning_levels"`
			BaseInstructions string         `json:"base_instructions"`
			ModelMessages    map[string]any `json:"model_messages"`
		} `json:"models"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	wantFirst := []string{"gpt-6.10-luna", "gpt-6.2-astra", "gpt-6.2-sol", "gpt-6.2-sol[1m]", "gpt-6.2-terra", "gpt-6.2-luna", "gpt-6.1-sol"}
	if len(catalog.Models) < len(wantFirst) {
		t.Fatalf("catalog has %d models, want at least %d", len(catalog.Models), len(wantFirst))
	}
	for i, slug := range wantFirst {
		if catalog.Models[i].Slug != slug {
			t.Fatalf("catalog position %d = %q, want %q", i, catalog.Models[i].Slug, slug)
		}
	}
	counts := make(map[string]int)
	var otherModels []string
	for i, model := range catalog.Models {
		if model.Priority != i+1 {
			t.Fatalf("catalog priority for %s = %d, want %d", model.Slug, model.Priority, i+1)
		}
		if !strings.HasPrefix(model.Slug, "gpt-") {
			otherModels = append(otherModels, model.Slug)
		} else if len(otherModels) > 0 {
			t.Fatalf("GPT model %s appears after %s", model.Slug, otherModels[0])
		}
		counts[model.Slug]++
		if expected, ok := discovered.Models[model.Slug]; ok {
			if model.Name != expected.DisplayName || model.ContextWindow != expected.ContextWindow || model.MaxContext != expected.ContextWindow ||
				model.Visibility != "list" || model.DefaultEffort != expected.DefaultReasoningEffort || len(model.ReasoningLevels) != len(expected.ReasoningEfforts) ||
				model.BaseInstructions != "You are Codex on "+model.Slug+"." || model.ModelMessages["instructions_template"] != model.Slug {
				t.Fatalf("discovered model metadata = %+v", model)
			}
			for i, level := range model.ReasoningLevels {
				if level.Effort != expected.ReasoningEfforts[i] {
					t.Fatalf("discovered reasoning level = %q, want %q", level.Effort, expected.ReasoningEfforts[i])
				}
			}
		}
		if model.Slug == "gpt-6.1-sol" && (model.Name != "Upstream GPT" || model.ContextWindow != 500000 || model.MaxContext != 872000) {
			t.Fatalf("upstream model metadata was rewritten: %+v", model)
		}
	}
	for _, id := range []string{"gpt-6.1-sol", "gpt-6.2-sol", "gpt-6.2-astra"} {
		if counts[id] != 1 {
			t.Fatalf("catalog has %d entries for %s", counts[id], id)
		}
	}
	if counts["gpt-6.2-terra-preview"] != 0 {
		t.Fatal("Codex model without an upstream catalog entry was listed")
	}
	if counts["grok-chat-only"] != 0 {
		t.Fatal("chat-only model included in the Codex catalog")
	}
	if len(otherModels) < 2 || otherModels[0] != "upstream-other-a" || otherModels[1] != "upstream-other-b" {
		t.Fatalf("other models did not retain their order: %v", otherModels)
	}
}

func upstreamCodexEntry(t *testing.T, model DiscoveredModel) json.RawMessage {
	t.Helper()
	levels := []map[string]string{}
	for _, effort := range model.ReasoningEfforts {
		levels = append(levels, map[string]string{"effort": effort, "description": effort})
	}
	raw, err := json.Marshal(map[string]any{
		"slug": model.ID, "display_name": model.DisplayName, "visibility": "list",
		"context_window": model.ContextWindow, "max_context_window": model.ContextWindow,
		"default_reasoning_level": model.DefaultReasoningEffort, "supported_reasoning_levels": levels,
		"base_instructions": "You are Codex on " + model.ID + ".", "model_messages": map[string]any{"instructions_template": model.ID},
	})
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

func TestRunnerCatalogServesUpstreamCodexEntries(t *testing.T) {
	entry := json.RawMessage(`{"slug":"gpt-6.1-sol","display_name":"GPT-6.1 Sol","context_window":500000,"max_context_window":872000,"base_instructions":"You are Codex.","priority":4}`)
	catalog := runnerCatalog([]poolModelDescriptor{
		{ID: "gpt-6.1-sol", Provider: "codex", Protocol: "openai", CodexCatalogEntry: entry},
		{ID: "gpt-6.1-sol[1m]", Name: "GPT-6.1 Sol 1M", Description: "1M context", Provider: "codex", Protocol: "openai", ContextWindow: 1000000, CodexCatalogEntry: entry},
		{ID: "gpt-6.2-sol", Provider: "codex", Protocol: "openai"},
	})
	if len(catalog.Models) != 2 || runnerproto.SupportsModel(catalog.Models, "codex", "gpt-6.2-sol", "") {
		t.Fatalf("runner models = %+v", catalog.Models)
	}
	var native struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal([]byte(catalog.CodexCatalog), &native); err != nil || len(native.Models) != 2 {
		t.Fatalf("Codex native catalog = %s", catalog.CodexCatalog)
	}
	base, variant := native.Models[0], native.Models[1]
	if base["slug"] != "gpt-6.1-sol" || base["display_name"] != "GPT-6.1 Sol" || base["context_window"] != float64(500000) || base["max_context_window"] != float64(872000) {
		t.Fatalf("upstream entry was rewritten: %v", base)
	}
	if variant["slug"] != "gpt-6.1-sol[1m]" || variant["display_name"] != "GPT-6.1 Sol 1M" || variant["description"] != "1M context" ||
		variant["context_window"] != float64(1000000) || variant["max_context_window"] != float64(1000000) {
		t.Fatalf("context variant entry = %v", variant)
	}
	for _, model := range native.Models {
		if model["base_instructions"] != "You are Codex." {
			t.Fatalf("Codex entry lost base instructions: %v", model)
		}
	}
}

func TestCodexModelsEndpointServesStaticGPTModelsFromDiscovery(t *testing.T) {
	t.Setenv("POOL_JWT_SECRET", "codex-catalog-static-secret")
	upstream := &Account{ID: "upstream", Type: AccountTypeCodex, AccessToken: "codex-access", AccountID: "acct-upstream", PlanType: "plus"}
	discovered := &Account{ID: "discovered", Type: AccountTypeCodex, AccessToken: "codex-discovered", AccountID: "acct-discovered", PlanType: "pro",
		Models: map[string]DiscoveredModel{"gpt-5.6-sol": {ID: "gpt-5.6-sol", DisplayName: "GPT-5.6 Sol", ContextWindow: 372000}},
	}
	model := discovered.Models["gpt-5.6-sol"]
	model.CatalogEntry = upstreamCodexEntry(t, model)
	discovered.Models["gpt-5.6-sol"] = model
	h := qwenTestHandler(t, roundTripFunc(func(req *http.Request) (*http.Response, error) {
		return qwenJSONResponse(200, `{"models":[{"slug":"gpt-6.1-sol","display_name":"GPT","base_instructions":"You are Codex on gpt-6.1-sol."}]}`), nil
	}), upstream, discovered)
	req := httptest.NewRequest("GET", "/backend-api/codex/models?client_version=0.164.0", nil)
	req.Header.Set("Authorization", "Bearer "+generateClaudePoolToken("codex-catalog-static-secret", "codex-client"))
	rr := httptest.NewRecorder()
	testPoolProxyRequest(t, h, rr, req, "codex-catalog")
	if rr.Code != http.StatusOK {
		t.Fatalf("status=%d catalog=%.400s", rr.Code, rr.Body.String())
	}
	var catalog struct {
		Models []map[string]any `json:"models"`
	}
	if err := json.Unmarshal(rr.Body.Bytes(), &catalog); err != nil {
		t.Fatal(err)
	}
	found := map[string]map[string]any{}
	for _, entry := range catalog.Models {
		found[stringValue(entry["slug"])] = entry
	}
	for _, slug := range []string{"gpt-5.6-sol", "gpt-5.6-sol[1m]"} {
		if found[slug] == nil || found[slug]["base_instructions"] != "You are Codex on gpt-5.6-sol." {
			t.Fatalf("%s entry = %v", slug, found[slug])
		}
	}
	if found["gpt-5.6-sol[1m]"]["context_window"] != float64(1000000) || found["gpt-5.6-sol"]["context_window"] != float64(372000) {
		t.Fatalf("context windows = %v / %v", found["gpt-5.6-sol"]["context_window"], found["gpt-5.6-sol[1m]"]["context_window"])
	}
	for _, slug := range []string{"gpt-5.5", "gpt-6-astra[1m]"} {
		if found[slug] != nil {
			t.Fatalf("%s listed without an upstream catalog entry: %v", slug, found[slug])
		}
	}
}
