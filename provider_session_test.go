package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func providerSessionRequest(t *testing.T, format contextWireFormat, text string, fields map[string]any) []byte {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(contextTestBody(format, text, true), &root); err != nil {
		t.Fatal(err)
	}
	for key, value := range fields {
		root[key] = value
	}
	encoded, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestProviderSessionsAreIsolatedAcrossTransitions(t *testing.T) {
	store := newConversationHandoffStore()
	initial := providerSessionRequest(t, contextFormatResponses, "first", map[string]any{
		"sessionId": "codex-native", "previous_response_id": "resp-codex", "prompt_cache_key": "cache-codex",
	})
	unchanged, result, err := store.Prepare("same-conversation", AccountTypeCodex, "/v1/responses", initial)
	if err != nil || result.Switched || string(unchanged) != string(initial) {
		t.Fatalf("direct provider request changed: switched=%v err=%v", result.Switched, err)
	}

	antigravity := providerSessionRequest(t, contextFormatResponses, "second", map[string]any{
		"previous_response_id": "resp-codex-new", "prompt_cache_key": "cache-codex-new",
	})
	out, result, err := store.Prepare("same-conversation", AccountTypeAntigravity, "/v1/responses", antigravity)
	if err != nil || !result.Switched || strings.Contains(string(out), "resp-codex-new") || strings.Contains(string(out), "cache-codex-new") {
		t.Fatalf("Codex state reached Antigravity: switched=%v err=%v payload=%s", result.Switched, err, out)
	}
	state, ok := store.State("same-conversation")
	if !ok || state.ID != "same-conversation" || state.ActiveProvider != AccountTypeAntigravity || state.TransitionEpoch != 1 {
		t.Fatalf("conversation identity or epoch changed incorrectly: %+v", state)
	}
	codex := state.ProviderSessions[AccountTypeCodex]
	anti := state.ProviderSessions[AccountTypeAntigravity]
	if codex == nil || codex.ResponseID != "resp-codex-new" || codex.CacheKey != "cache-codex-new" || codex.NativeSessionID != "codex-native" {
		t.Fatalf("Codex native state lost: %+v", codex)
	}
	if anti == nil || anti.Epoch != 1 || anti.ResponseID != "" || anti.NativeSessionID != "" || anti.CacheKey != "" {
		t.Fatalf("new Antigravity epoch inherited Codex state: %+v", anti)
	}

	// Native Antigravity continuation may use its own upstream identifiers.
	antigravityNext := providerSessionRequest(t, contextFormatResponses, "third", map[string]any{
		"sessionId": "-123456", "response_id": "anti-response", "prompt_cache_key": "anti-cache",
	})
	unchanged, result, err = store.Prepare("same-conversation", AccountTypeAntigravity, "/v1/responses", antigravityNext)
	if err != nil || result.Switched || string(unchanged) != string(antigravityNext) {
		t.Fatalf("native continuation changed: switched=%v err=%v", result.Switched, err)
	}
	state, _ = store.State("same-conversation")
	anti = state.ProviderSessions[AccountTypeAntigravity]
	if anti.NativeSessionID != "-123456" || anti.ResponseID != "anti-response" || anti.CacheKey != "anti-cache" || anti.Epoch != 1 {
		t.Fatalf("Antigravity native state not retained: %+v", anti)
	}

	zai := providerSessionRequest(t, contextFormatClaude, "fourth", map[string]any{"sessionId": "-123456"})
	out, result, err = store.Prepare("same-conversation", AccountTypeZAI, "/v1/messages", zai)
	if err != nil || !result.Switched || strings.Contains(string(out), "-123456") {
		t.Fatalf("Antigravity native session reached ZAI: switched=%v err=%v payload=%s", result.Switched, err, out)
	}
	state, _ = store.State("same-conversation")
	if state.ActiveProvider != AccountTypeZAI || state.TransitionEpoch != 2 || state.ProviderSessions[AccountTypeZAI].Epoch != 2 {
		t.Fatalf("ZAI transition state = %+v", state)
	}
	if state.ProviderSessions[AccountTypeZAI].NativeSessionID != "" {
		t.Fatalf("ZAI inherited Antigravity native session: %+v", state.ProviderSessions[AccountTypeZAI])
	}
}
