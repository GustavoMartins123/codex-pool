package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
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
