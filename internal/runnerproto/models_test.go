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

func TestModelRevisionIsStableAcrossReportOrdering(t *testing.T) {
	first := []AgentModel{{Agent: "codex", Model: "b", ReasoningEfforts: []string{"high", "low"}}, {Agent: "claude", Model: "a"}}
	second := []AgentModel{first[1], {Agent: "codex", Model: "b", ReasoningEfforts: []string{"low", "high"}}}
	if ModelsRevision(first) != ModelsRevision(second) {
		t.Fatal("report order changed the revision")
	}
	if first[0].ReasoningEfforts[0] != "high" {
		t.Fatal("hashing mutated the source report")
	}
	if ModelsRevision(nil) != ModelsRevision([]AgentModel{}) {
		t.Fatal("empty reports have different revisions")
	}
	capabilities := &ModelCapabilities{Revision: ModelsRevision(first), Models: first}
	if ValidateCapabilities(capabilities) != nil {
		t.Fatal("valid revision rejected")
	}
	capabilities.Models[0].Model = "changed"
	if ValidateCapabilities(capabilities) == nil {
		t.Fatal("changed content kept its old revision")
	}
}
