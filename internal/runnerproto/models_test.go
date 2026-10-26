package runnerproto

import "testing"

func TestAgentModelCapabilitiesRejectAmbiguousOrInvalidReports(t *testing.T) {
	valid := AgentModel{Agent: "codex", Model: "pool-only", ReasoningEfforts: []string{"low", "high"}, DefaultReasoningEffort: "high"}
	if err := ValidateAgentModels([]AgentModel{valid}); err != nil {
		t.Fatal(err)
	}
	for _, models := range [][]AgentModel{
		{valid, valid},
		{{Agent: "unknown", Model: "model"}},
		{{Agent: "codex", Model: ""}},
		{{Agent: "codex", Model: "model", ReasoningEfforts: []string{"invalid"}}},
		{{Agent: "codex", Model: "model", ReasoningEfforts: []string{"high", "high"}}},
		{{Agent: "codex", Model: "model", ReasoningEfforts: []string{"low"}, DefaultReasoningEffort: "high"}},
		{{Agent: "claude", Model: "model", ReasoningEfforts: []string{"minimal"}}},
	} {
		if err := ValidateAgentModels(models); err == nil {
			t.Fatalf("accepted invalid capabilities: %+v", models)
		}
	}
	if !SupportsModel([]AgentModel{valid}, "codex", "pool-only", "high") || SupportsModel([]AgentModel{valid}, "codex", "pool-only", "max") || SupportsModel([]AgentModel{valid}, "claude", "pool-only", "") {
		t.Fatal("model support ignored agent or effort")
	}
}
