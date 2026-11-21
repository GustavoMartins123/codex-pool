package main

import (
	"encoding/json"
	"strings"
	"testing"

	"codex-pool-proxy/internal/wire"
)

func TestCanTransitionAccountsForHistoryAndCurrentContent(t *testing.T) {
	state := ConversationState{ActiveProvider: AccountTypeCodex}
	state.IR = conversationIRFromMessages([]Message{transitionText("user", "hello")}, "conv", 0)
	if plan := CanTransition(state, AccountTypeCodex, AccountTypeCodex, nil); !plan.Allowed || plan.Cost != 0 || plan.Mode != TransitionNative {
		t.Fatalf("native plan=%+v", plan)
	}
	if plan := CanTransition(state, AccountTypeCodex, AccountTypeAntigravity, nil); !plan.Allowed || plan.Cost != 0.3 || plan.Mode != TransitionSafeHistory {
		t.Fatalf("safe plan=%+v", plan)
	}
	if plan := CanTransition(state, AccountTypeClaude, AccountTypeZAI, nil); !plan.Allowed || plan.Cost != 0.1 || plan.Mode != TransitionFullHistory {
		t.Fatalf("full plan=%+v", plan)
	}
	state.IR = conversationIRFromMessages([]Message{{Role: "user", Parts: []MessagePart{{Type: "image", ImageURL: "data:image/png;base64,aGVsbG8="}}}}, "conv", 0)
	if plan := CanTransition(state, AccountTypeCodex, AccountTypeZAI, nil); plan.Allowed || plan.Mode != TransitionSummary {
		t.Fatalf("unsupported current image allowed: %+v", plan)
	}
	state.IR = conversationIRFromMessages([]Message{{Role: "tool", Parts: []MessagePart{{Type: "tool_result", ToolID: "call", Text: "done"}}}}, "conv", 0)
	if plan := CanTransition(state, AccountTypeCodex, AccountType("unknown"), nil); plan.Allowed {
		t.Fatalf("orphan current tool result allowed: %+v", plan)
	}
}

func TestProviderTransitionRebuildsPayloadForTargetProtocol(t *testing.T) {
	store := newConversationHandoffStore()
	initial := contextTestBody(contextFormatResponses, "prior turn", false)
	if _, _, err := store.Prepare(conversationScopedKey("user", "provider-switch-conversation"), AccountTypeCodex, "/v1/responses", initial); err != nil {
		t.Fatal(err)
	}
	store.RecordAssistantText(conversationScopedKey("user", "provider-switch-conversation"), AccountTypeCodex, "prior answer")
	source := addContextOpaqueState(t, []byte(`{"model":"claude-sonnet-5","input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"current request"}]}]}`))
	clean, result, err := store.Prepare(conversationScopedKey("user", "provider-switch-conversation"), AccountTypeClaude, "/v1/responses", source)
	if err != nil || !result.Switched {
		t.Fatalf("handoff=%+v err=%v", result, err)
	}
	translated, err := wire.ResponsesRequestToAnthropic(clean, claudeCanonicalModel)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(translated), "resp_previous_provider") || strings.Contains(string(translated), "opaque-reasoning") {
		t.Fatalf("foreign state reached another provider: %s", translated)
	}
	var root map[string]any
	if err := json.Unmarshal(translated, &root); err != nil {
		t.Fatal(err)
	}
	visible := normalizedContextText(t, "/v1/messages", translated)
	for _, want := range []string{"prior turn", "prior answer", "current request"} {
		if !strings.Contains(visible, want) {
			t.Fatalf("provider switch lost %q: %s", want, translated)
		}
	}
}
