package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func contextTestBody(format contextWireFormat, text string, stream bool) []byte {
	var body map[string]any
	switch format {
	case contextFormatResponses:
		body = map[string]any{
			"model": "test-model", "stream": stream,
			"input": []any{map[string]any{
				"type": "message", "role": "user",
				"content": []any{map[string]any{"type": "input_text", "text": text}},
			}},
		}
	case contextFormatOpenAI:
		body = map[string]any{
			"model": "test-model", "stream": stream,
			"messages": []any{map[string]any{"role": "user", "content": text}},
		}
	case contextFormatClaude:
		body = map[string]any{
			"model": "test-model", "stream": stream, "max_tokens": 1024,
			"messages": []any{map[string]any{
				"role": "user", "content": []any{map[string]any{"type": "text", "text": text}},
			}},
		}
	case contextFormatGemini:
		body = map[string]any{
			"model": "test-model",
			"contents": []any{map[string]any{
				"role": "user", "parts": []any{map[string]any{"text": text}},
			}},
		}
	}
	encoded, _ := json.Marshal(body)
	return encoded
}

func contextTestPath(format contextWireFormat) string {
	switch format {
	case contextFormatResponses:
		return "/v1/responses"
	case contextFormatOpenAI:
		return "/v1/chat/completions"
	case contextFormatClaude:
		return "/v1/messages"
	case contextFormatGemini:
		return "/v1beta/models/test:generateContent"
	default:
		return "/"
	}
}

func addContextOpaqueState(t *testing.T, body []byte) []byte {
	t.Helper()
	var object map[string]any
	if err := json.Unmarshal(body, &object); err != nil {
		t.Fatal(err)
	}
	object["previous_response_id"] = "resp_previous_provider"
	object["prompt_cache_key"] = "cache_previous_provider"
	object["sessionId"] = "session_previous_provider"
	object["reasoning"] = map[string]any{
		"effort": "high", "encrypted_content": "opaque-reasoning",
	}
	encoded, _ := json.Marshal(object)
	return encoded
}

func normalizedContextText(t *testing.T, path string, body []byte) string {
	t.Helper()
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		t.Fatalf("decode context body: %v", err)
	}
	object := contextRequestObject(root)
	format := detectContextWireFormat(path, object)
	messages := normalizeConversationMessages(format, object)
	var text []string
	for _, message := range messages {
		for _, part := range message.Parts {
			if part.Text != "" {
				text = append(text, part.Text)
			}
		}
	}
	return strings.Join(text, "\n")
}

func TestUniversalContextHandoffProviderMatrix(t *testing.T) {
	tests := []struct {
		name         string
		from         AccountType
		to           AccountType
		firstFormat  contextWireFormat
		secondFormat contextWireFormat
	}{
		{name: "Codex to Antigravity", from: AccountTypeCodex, to: AccountTypeAntigravity, firstFormat: contextFormatResponses, secondFormat: contextFormatResponses},
		{name: "Antigravity to Codex", from: AccountTypeAntigravity, to: AccountTypeCodex, firstFormat: contextFormatGemini, secondFormat: contextFormatResponses},
		{name: "Codex to Claude", from: AccountTypeCodex, to: AccountTypeClaude, firstFormat: contextFormatResponses, secondFormat: contextFormatClaude},
		{name: "Claude to Gemini", from: AccountTypeClaude, to: AccountTypeGemini, firstFormat: contextFormatClaude, secondFormat: contextFormatGemini},
		{name: "Gemini to Codex", from: AccountTypeGemini, to: AccountTypeCodex, firstFormat: contextFormatGemini, secondFormat: contextFormatResponses},
		{name: "Claude to Antigravity", from: AccountTypeClaude, to: AccountTypeAntigravity, firstFormat: contextFormatClaude, secondFormat: contextFormatResponses},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			store := newConversationHandoffStore()
			conversationID := "conversation-" + strings.ReplaceAll(test.name, " ", "-")
			first := contextTestBody(test.firstFormat, "first-provider-turn", true)
			if _, result, err := store.Prepare(conversationID, test.from, contextTestPath(test.firstFormat), first); err != nil {
				t.Fatalf("prepare first provider: %v", err)
			} else if result.Switched {
				t.Fatal("first request must not be marked as a switch")
			}

			second := addContextOpaqueState(t, contextTestBody(test.secondFormat, "second-provider-turn", true))
			rewritten, result, err := store.Prepare(conversationID, test.to, contextTestPath(test.secondFormat), second)
			if err != nil {
				t.Fatalf("prepare provider switch: %v", err)
			}
			if !result.Switched {
				t.Fatal("provider transition was not detected")
			}
			if !strings.Contains(strings.Join(result.Warnings, ","), "opaque_provider_state_dropped") {
				t.Fatalf("warnings = %v, want opaque provider state warning", result.Warnings)
			}
			for _, forbidden := range []string{
				"previous_response_id", "prompt_cache_key", "session_previous_provider", "opaque-reasoning",
			} {
				if strings.Contains(string(rewritten), forbidden) {
					t.Fatalf("rewritten request retained %q: %s", forbidden, rewritten)
				}
			}
			text := normalizedContextText(t, contextTestPath(test.secondFormat), rewritten)
			if !strings.Contains(text, "first-provider-turn") || !strings.Contains(text, "second-provider-turn") {
				t.Fatalf("normalized history was not transported: %q", text)
			}
			state, ok := store.State(conversationID)
			if !ok || len(state.Messages) < 2 {
				t.Fatalf("missing normalized state: %+v", state)
			}
			if local := state.ProviderState[string(test.from)]; len(local.Identifiers) == 0 {
				t.Fatalf("provider-local identifiers were not isolated under %q: %+v", test.from, state.ProviderState)
			}
		})
	}
}

func TestUniversalContextHandoffRegeneratesToolCallIDs(t *testing.T) {
	store := newConversationHandoffStore()
	first := []byte(`{
		"model":"claude-test",
		"messages":[
			{"role":"assistant","content":[{"type":"tool_use","id":"toolu_old","name":"lookup","input":{"q":"weather"}}]},
			{"role":"user","content":[{"type":"tool_result","tool_use_id":"toolu_old","content":"sunny"}]}
		]
	}`)
	if _, _, err := store.Prepare("tools-conversation", AccountTypeClaude, "/v1/messages", first); err != nil {
		t.Fatal(err)
	}
	second := addContextOpaqueState(t, contextTestBody(contextFormatResponses, "continue after tool", true))
	rewritten, result, err := store.Prepare("tools-conversation", AccountTypeCodex, "/v1/responses", second)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Switched {
		t.Fatal("expected provider switch")
	}
	if strings.Contains(string(rewritten), "toolu_old") {
		t.Fatalf("old provider tool ID leaked: %s", rewritten)
	}
	var root map[string]any
	if err := json.Unmarshal(rewritten, &root); err != nil {
		t.Fatal(err)
	}
	input, _ := root["input"].([]any)
	callID := ""
	resultID := ""
	for _, raw := range input {
		item, _ := raw.(map[string]any)
		switch item["type"] {
		case "function_call":
			callID = stringValue(item["call_id"])
		case "function_call_output":
			resultID = stringValue(item["call_id"])
		}
	}
	if callID == "" || callID != resultID || !strings.HasPrefix(callID, "call_") {
		t.Fatalf("regenerated tool IDs do not match: call=%q result=%q body=%s", callID, resultID, rewritten)
	}
}

func TestUniversalContextHandoffCompactsLargeConversation(t *testing.T) {
	store := newConversationHandoffStore()
	large := strings.Repeat("context-data-", 50_000)
	body := contextTestBody(contextFormatResponses, large, true)
	rewritten, result, err := store.Prepare("large-conversation", AccountTypeCodex, "/v1/responses", body)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Compacted {
		t.Fatal("conversation over 100k estimated tokens was not compacted")
	}
	state, ok := store.State("large-conversation")
	if !ok || state.Summary == "" {
		t.Fatalf("compacted state missing summary: %+v", state)
	}
	if tokens := estimateContextTokens(state.Messages); tokens >= contextHandoffCompactTokens {
		t.Fatalf("compacted state still estimates %d tokens", tokens)
	}
	if len(rewritten) >= len(body) {
		t.Fatalf("compaction did not reduce request size: before=%d after=%d", len(body), len(rewritten))
	}

	next := addContextOpaqueState(t, contextTestBody(contextFormatClaude, "after compaction", true))
	rewritten, result, err = store.Prepare("large-conversation", AccountTypeClaude, "/v1/messages", next)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Switched {
		t.Fatal("switch after compaction was not detected")
	}
	text := normalizedContextText(t, "/v1/messages", rewritten)
	if !strings.Contains(text, "Earlier conversation compacted") || !strings.Contains(text, "after compaction") {
		t.Fatalf("compacted handoff lost summary or latest turn: %q", text)
	}
}

func TestUniversalContextHandoffPreservesStreamingFlag(t *testing.T) {
	store := newConversationHandoffStore()
	first := contextTestBody(contextFormatResponses, "first", true)
	if _, _, err := store.Prepare("stream-conversation", AccountTypeCodex, "/v1/responses", first); err != nil {
		t.Fatal(err)
	}
	second := addContextOpaqueState(t, contextTestBody(contextFormatClaude, "second", true))
	rewritten, _, err := store.Prepare("stream-conversation", AccountTypeClaude, "/v1/messages", second)
	if err != nil {
		t.Fatal(err)
	}
	var object map[string]any
	if err := json.Unmarshal(rewritten, &object); err != nil {
		t.Fatal(err)
	}
	if stream, _ := object["stream"].(bool); !stream {
		t.Fatalf("streaming flag was not preserved: %s", rewritten)
	}
}

func TestUniversalContextHandoffWebSocketFrame(t *testing.T) {
	store := newConversationHandoffStore()
	first := contextTestBody(contextFormatClaude, "claude websocket predecessor", true)
	if _, _, err := store.Prepare("ws-conversation", AccountTypeClaude, "/v1/messages", first); err != nil {
		t.Fatal(err)
	}
	frame := []byte(`{
		"type":"response.create",
		"response":{
			"model":"gpt-test",
			"previous_response_id":"resp_claude",
			"stream":true,
			"input":[{"type":"message","role":"user","content":[{"type":"input_text","text":"websocket turn"}]}]
		}
	}`)
	rewritten, result, err := store.Prepare("ws-conversation", AccountTypeCodex, "/v1/responses", frame)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Switched || strings.Contains(string(rewritten), "previous_response_id") {
		t.Fatalf("websocket frame was not safely handed off: result=%+v frame=%s", result, rewritten)
	}
	var root map[string]any
	if err := json.Unmarshal(rewritten, &root); err != nil {
		t.Fatal(err)
	}
	if root["type"] != "response.create" {
		t.Fatalf("websocket envelope changed: %s", rewritten)
	}
	text := normalizedContextText(t, "/v1/responses", rewritten)
	if !strings.Contains(text, "claude websocket predecessor") || !strings.Contains(text, "websocket turn") {
		t.Fatalf("websocket history was not reconstructed: %q", text)
	}
}

func TestUniversalContextHandoffKeepsProviderLocalStateOnSameProvider(t *testing.T) {
	store := newConversationHandoffStore()
	body := addContextOpaqueState(t, contextTestBody(contextFormatResponses, "same provider", true))
	rewritten, result, err := store.Prepare("same-provider", AccountTypeCodex, "/v1/responses", body)
	if err != nil {
		t.Fatal(err)
	}
	if result.Switched || string(rewritten) != string(body) {
		t.Fatalf("same-provider state should remain untouched: result=%+v body=%s", result, rewritten)
	}
}

func TestUniversalContextHandoffCapturesAssistantResponse(t *testing.T) {
	store := newConversationHandoffStore()
	first := contextTestBody(contextFormatResponses, "first user turn", true)
	if _, _, err := store.Prepare("response-capture", AccountTypeCodex, "/v1/responses", first); err != nil {
		t.Fatal(err)
	}
	sample := []byte(
		"event: response.output_text.delta\n" +
			`data: {"type":"response.output_text.delta","delta":"assistant "}` + "\n\n" +
			"event: response.output_text.delta\n" +
			`data: {"type":"response.output_text.delta","delta":"answer"}` + "\n\n" +
			`data: {"type":"response.completed","response":{"id":"resp_1","output":[]}}` + "\n\n",
	)
	text := extractAssistantTextFromResponseSample(sample)
	if text != "assistant answer" {
		t.Fatalf("captured assistant text = %q", text)
	}
	store.RecordAssistantText("response-capture", AccountTypeCodex, text)

	next := addContextOpaqueState(t, contextTestBody(contextFormatClaude, "next provider turn", true))
	rewritten, _, err := store.Prepare("response-capture", AccountTypeClaude, "/v1/messages", next)
	if err != nil {
		t.Fatal(err)
	}
	history := normalizedContextText(t, "/v1/messages", rewritten)
	for _, expected := range []string{"first user turn", "assistant answer", "next provider turn"} {
		if !strings.Contains(history, expected) {
			t.Fatalf("handoff history missing %q: %q", expected, history)
		}
	}
}
