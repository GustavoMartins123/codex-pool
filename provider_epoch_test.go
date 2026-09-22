package main

import (
	"encoding/json"
	"testing"
	"time"
)

func TestTransitionEpochChangesRegeneratedToolIDs(t *testing.T) {
	store := newConversationHandoffStore()
	initial := transitionBody(t, contextFormatResponses, []Message{
		transitionText("user", "run lookup"),
		{Role: "assistant", Parts: []MessagePart{{Type: "tool_call", ToolID: "foreign-call", ToolName: "lookup", Arguments: `{"q":"x"}`}}},
		{Role: "tool", Parts: []MessagePart{{Type: "tool_result", ToolID: "foreign-call", Text: "found"}}},
		transitionText("assistant", "done"),
	})
	if _, _, err := store.Prepare("epoch-conversation", AccountTypeAntigravity, "/v1/responses", initial); err != nil {
		t.Fatal(err)
	}
	toCodex := contextTestBody(contextFormatResponses, "switch to Codex", false)
	out, result, err := store.Prepare("epoch-conversation", AccountTypeCodex, "/v1/responses", toCodex)
	if err != nil || !result.Switched {
		t.Fatalf("first switch: %+v %v", result, err)
	}
	firstID := firstTransitionToolID(t, out)
	toAnti := contextTestBody(contextFormatResponses, "switch back to Antigravity", false)
	out, result, err = store.Prepare("epoch-conversation", AccountTypeAntigravity, "/v1/responses", toAnti)
	if err != nil || !result.Switched {
		t.Fatalf("second switch: %+v %v", result, err)
	}
	secondID := firstTransitionToolID(t, out)
	if firstID == secondID {
		t.Fatalf("tool ID reused across epochs: %s", firstID)
	}
	state, _ := store.State("epoch-conversation")
	if state.TransitionEpoch != 2 || state.ProviderSessions[AccountTypeAntigravity].Epoch != 2 {
		t.Fatalf("epoch state=%+v", state)
	}
}

func firstTransitionToolID(t *testing.T, body []byte) string {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatal(err)
	}
	messages := normalizeResponsesContext(root)
	for _, message := range messages {
		for _, part := range message.Parts {
			if part.Type == "tool_call" {
				return part.ToolID
			}
		}
	}
	t.Fatalf("no tool call in %s", body)
	return ""
}

func TestAntigravityReplayCacheIsolatedByConversationAndEpoch(t *testing.T) {
	cache := newAntigravityReplayCache(time.Hour, 1024)
	request := replayTestEnvelope("gemini-3.1-flash-lite", "same-native-session", []any{
		map[string]any{"role": "user", "parts": []any{map[string]any{"text": "read"}}},
	})
	scope := antigravityReplayScopeFromBody(request)
	scope.ConversationID, scope.Provider, scope.Epoch = "conversation-a", AccountTypeAntigravity, 1
	response := []byte(`{"response":{"candidates":[{"content":{"parts":[{"functionCall":{"id":"call-1","name":"read","args":{}},"thoughtSignature":"epoch-1-signature"}]}}]}}`)
	if !cache.capture(scope, request, response) {
		t.Fatal("native replay capture failed")
	}
	if _, ok := cache.get(scope, time.Now()); !ok {
		t.Fatal("same epoch lost native replay")
	}
	otherEpoch := scope
	otherEpoch.Epoch = 3
	if _, ok := cache.get(otherEpoch, time.Now()); ok {
		t.Fatal("old replay reached a new epoch")
	}
	otherConversation := scope
	otherConversation.ConversationID = "conversation-b"
	if _, ok := cache.get(otherConversation, time.Now()); ok {
		t.Fatal("native replay reached another conversation")
	}
}
