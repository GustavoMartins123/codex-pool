package main

import (
	"reflect"
	"testing"
)

func TestCodexReasoningCapabilitiesComeFromAllAuthorizedAccounts(t *testing.T) {
	body := []byte(`{"models":[{"slug":"gpt-6.1-sol","supported_reasoning_levels":[{"effort":"low"},{"effort":"high"},{"effort":"xhigh"}],"default_reasoning_level":"high"}]}`)
	parsed, err := parseProviderModels(body)
	if err != nil {
		t.Fatal(err)
	}
	first := &Account{ID: "first", Type: AccountTypeCodex, Models: parsed}
	second := &Account{ID: "second", Type: AccountTypeCodex, Models: map[string]DiscoveredModel{
		"gpt-6.1-sol": {ID: "gpt-6.1-sol", Reasoning: true, ReasoningEfforts: []string{"low", "high"}, DefaultReasoningEffort: "high"},
	}}
	pool := newPoolState([]*Account{first, second}, false)
	model := poolModelDescriptor{ID: "gpt-6.1-sol[1m]", Provider: string(AccountTypeCodex), Capabilities: map[string]bool{"reasoning": true}}
	model.ReasoningEfforts = modelReasoningEfforts(pool, model)
	if !reflect.DeepEqual(model.ReasoningEfforts, []string{"low", "high"}) {
		t.Fatalf("efforts=%v", model.ReasoningEfforts)
	}
	if got := modelDefaultReasoningEffort(pool, model); got != "high" {
		t.Fatalf("default=%q", got)
	}
	second.Models["gpt-6.1-sol"] = DiscoveredModel{ID: "gpt-6.1-sol", Reasoning: true}
	if got := modelReasoningEfforts(pool, model); len(got) != 0 {
		t.Fatalf("invented capabilities: %v", got)
	}
}

func TestReasoningEffortsFollowProviderAndModel(t *testing.T) {
	for _, tc := range []struct {
		provider AccountType
		id       string
		want     []string
	}{
		{AccountTypeClaude, "claude-sonnet-5-5", []string{"low", "medium", "high", "xhigh", "max"}},
		{AccountTypeClaude, "claude-sonnet-4-6", []string{"low", "medium", "high", "max"}},
		{AccountTypeClaude, "claude-haiku-4-5-20251001", []string{}},
		{AccountTypeAntigravity, "antigravity/gemini-3-pro-preview", []string{"low", "high"}},
		{AccountTypeAntigravity, "antigravity/gemini-3-flash-preview", []string{"minimal", "low", "medium", "high"}},
		{AccountTypeGemini, "unknown-model", []string{}},
		{AccountTypeKimi, "kimi-for-coding", []string{}},
	} {
		t.Run(tc.id, func(t *testing.T) {
			got := modelReasoningEfforts(nil, poolModelDescriptor{ID: tc.id, Provider: string(tc.provider), Capabilities: map[string]bool{"reasoning": true}})
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("got=%v want=%v", got, tc.want)
			}
		})
	}
}

func TestAnthropicEffortReachesTheGeminiThinkingLevel(t *testing.T) {
	converted, err := antigravityAnthropicToGemini(map[string]any{
		"messages":      []any{map[string]any{"role": "user", "content": "synthetic"}},
		"thinking":      map[string]any{"type": "adaptive"},
		"output_config": map[string]any{"effort": "high"},
	})
	if err != nil {
		t.Fatal(err)
	}
	thinking := mapValue(mapValue(converted["generationConfig"])["thinkingConfig"])
	if thinking["thinkingLevel"] != "high" {
		t.Fatalf("effort was lost: %+v", thinking)
	}
	if _, invented := thinking["thinkingBudget"]; invented {
		t.Fatal("effort was replaced with an invented token budget")
	}
}
