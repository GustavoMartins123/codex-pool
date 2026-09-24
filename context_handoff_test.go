package main

import (
	"encoding/json"
	"net/http/httptest"
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
			{"role":"user","content":[{"type":"text","text":"look up weather"}]},
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

func TestSanitizeConversationToolPairsBackfillsToolResultName(t *testing.T) {
	messages := []Message{
		{Role: "user", Parts: []MessagePart{{Type: "text", Text: "trigger tool"}}},
		{Role: "assistant", Parts: []MessagePart{{Type: "tool_call", ToolID: "call-1", ToolName: "lookup", Arguments: "{}"}}},
		{Role: "tool", Parts: []MessagePart{{Type: "tool_result", ToolID: "call-1", Text: "ok"}}},
	}
	cleaned, warnings := sanitizeConversationToolPairs(messages)
	if len(warnings) != 0 {
		t.Fatalf("unexpected warnings: %v", warnings)
	}
	found := false
	for _, message := range cleaned {
		for _, part := range message.Parts {
			if part.Type == "tool_result" {
				found = true
				if part.ToolName != "lookup" {
					t.Fatalf("tool result name = %q", part.ToolName)
				}
			}
		}
	}
	if !found {
		t.Fatal("tool result was dropped")
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
	if result.Compacted {
		t.Fatal("same-provider request must not be rewritten by pool compaction")
	}
	if string(rewritten) != string(body) {
		t.Fatal("same-provider large request was mutated")
	}
	state, ok := store.State("large-conversation")
	if !ok || state.Summary == "" {
		t.Fatalf("internally compacted state missing summary: %+v", state)
	}
	if tokens := estimateContextTokens(state.Messages); tokens >= contextHandoffCompactTokens {
		t.Fatalf("internally compacted state still estimates %d tokens", tokens)
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

func TestAntigravitySameProviderLargeRequestIsCompacted(t *testing.T) {
	old := strings.Repeat("old-context-", 260_000)
	body, err := json.Marshal(map[string]any{
		"model":  "antigravity/gemini-3.8-flash-high",
		"stream": true,
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": old}}},
			map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "old answer"}}},
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "latest request"}}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	h := &proxyHandler{}
	w := httptest.NewRecorder()
	rewritten, err := h.prepareProviderContextHandoff(w, "antigravity-large", AccountTypeAntigravity, "/v1/responses", body)
	if err != nil {
		t.Fatal(err)
	}
	if w.Header().Get("X-Pool-Context-Compacted") != "true" {
		t.Fatal("large Antigravity request was not marked as compacted")
	}
	var root map[string]any
	if err := json.Unmarshal(rewritten, &root); err != nil {
		t.Fatal(err)
	}
	if root["stream"] != true {
		t.Fatalf("stream flag was not preserved: %s", rewritten)
	}
	messages := normalizeConversationMessages(contextFormatResponses, root)
	if tokens := estimateContextTokens(messages); tokens >= antigravityWireCompactTokens {
		t.Fatalf("compacted request still estimates %d tokens", tokens)
	}
	text := normalizedContextText(t, "/v1/responses", rewritten)
	if !strings.Contains(text, "Earlier conversation compacted") || !strings.Contains(text, "latest request") {
		t.Fatalf("compaction lost the summary or latest turn: %q", text)
	}
}

func TestContextTokenEstimateIncludesImagePayload(t *testing.T) {
	image := []Message{{Role: "user", Parts: []MessagePart{{Type: "image", Data: strings.Repeat("x", 100_000)}}}}
	empty := []Message{{Role: "user", Parts: []MessagePart{{Type: "image"}}}}
	if estimateContextTokens(image) <= estimateContextTokens(empty) {
		t.Fatal("image payload was not included in context estimation")
	}
}

func TestContextHandoffTrimKeepsToolExchangeAtomic(t *testing.T) {
	messages := []Message{
		{Role: "user", Parts: []MessagePart{{Type: "text", Text: "trigger tool"}}},
		{Role: "assistant", Parts: []MessagePart{{Type: "tool_call", ToolID: "call_atomic", ToolName: "lookup", Arguments: "{\"q\":\"x\"}"}}},
		{Role: "tool", Parts: []MessagePart{{Type: "tool_result", ToolID: "call_atomic", Text: strings.Repeat("result-", 200)}}},
		{Role: "user", Parts: []MessagePart{{Type: "text", Text: strings.Repeat("latest-", 200)}}},
	}

	// Budget fits the latest user message but not the preceding tool exchange.
	trimmed := trimConversationMessages(messages, messageCharacterSize(messages[3])+64)
	for _, message := range trimmed {
		for _, part := range message.Parts {
			if part.ToolID == "call_atomic" {
				t.Fatalf("tool pair was cut in half instead of being dropped atomically: %+v", trimmed)
			}
		}
	}
	if len(trimmed) == 0 || trimmed[len(trimmed)-1].Role != "user" {
		t.Fatalf("latest user turn was not retained: %+v", trimmed)
	}

	// With enough room, the trigger + call + result must survive together.
	budget := 0
	for _, message := range messages[0:4] {
		budget += messageCharacterSize(message)
	}
	trimmed = trimConversationMessages(messages, budget)
	var callSeen, resultSeen bool
	for _, message := range trimmed {
		for _, part := range message.Parts {
			if part.Type == "tool_call" && part.ToolID == "call_atomic" {
				callSeen = true
			}
			if part.Type == "tool_result" && part.ToolID == "call_atomic" {
				resultSeen = true
			}
		}
	}
	if !callSeen || !resultSeen {
		t.Fatalf("complete tool exchange was not retained: %+v", trimmed)
	}
}

func TestContextHandoffDropsInvalidToolPrefixAfterCompaction(t *testing.T) {
	messages := []Message{
		{Role: "assistant", Parts: []MessagePart{{Type: "tool_call", ToolID: "call_bad", ToolName: "lookup", Arguments: "{}"}}},
		{Role: "tool", Parts: []MessagePart{{Type: "tool_result", ToolID: "call_bad", Text: "result"}}},
		{Role: "user", Parts: []MessagePart{{Type: "text", Text: "continue"}}},
	}
	cleaned, warnings := sanitizeConversationToolPairs(messages)
	if !strings.Contains(strings.Join(warnings, ","), "invalid_tool_call_prefix_dropped") {
		t.Fatalf("expected invalid prefix warning, got %v", warnings)
	}
	for _, message := range cleaned {
		for _, part := range message.Parts {
			if part.Type == "tool_call" || part.Type == "tool_result" {
				t.Fatalf("invalid leading tool exchange survived sanitization: %+v", cleaned)
			}
		}
	}
}

func TestSameProviderLargeToolHistoryIsNotRewritten(t *testing.T) {
	store := newConversationHandoffStore()
	large := strings.Repeat("history-", 60_000)
	body := []byte(`{
		"model":"gpt-test",
		"input":[
			{"type":"message","role":"user","content":[{"type":"input_text","text":"` + large + `"}]},
			{"type":"function_call","call_id":"call_live","name":"lookup","arguments":"{}"},
			{"type":"function_call_output","call_id":"call_live","output":"ok"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}
		]
	}`)
	rewritten, result, err := store.Prepare("same-provider-large-tools", AccountTypeCodex, "/v1/responses", body)
	if err != nil {
		t.Fatal(err)
	}
	if result.Switched || result.Compacted {
		t.Fatalf("same-provider request should be transparent: %+v", result)
	}
	if string(rewritten) != string(body) {
		t.Fatal("same-provider tool history was rewritten")
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

func TestFailedTransitionRetriesAsSwitch(t *testing.T) {
	store := newConversationHandoffStore()
	convID := "failed-transition-retry"

	first := contextTestBody(contextFormatResponses, "first user turn", true)
	if _, _, err := store.Prepare(convID, AccountTypeCodex, "/v1/responses", first); err != nil {
		t.Fatal(err)
	}
	store.RecordAssistantText(convID, AccountTypeCodex, "first answer")
	store.MarkNativeSessionEstablished(convID, AccountTypeCodex)

	second := addContextOpaqueState(t, contextTestBody(contextFormatResponses, "second turn on gemini", true))
	out2, result2, err := store.Prepare(convID, AccountTypeAntigravity, "/v1/responses", second)
	if err != nil || !result2.Switched {
		t.Fatalf("turn 2 should switch: switched=%v err=%v", result2.Switched, err)
	}

	// Turn 2 fails with 429
	store.MarkTransitionOutcome(convID, result2.Epoch, 429, ProviderErrorQuota, false)

	// Turn 3 retries or continues with the same client state
	third := addContextOpaqueState(t, contextTestBody(contextFormatResponses, "third turn retry", true))
	out3, result3, err := store.Prepare(convID, AccountTypeAntigravity, "/v1/responses", third)
	if err != nil {
		t.Fatal(err)
	}
	if !result3.Switched || result3.From != AccountTypeCodex {
		t.Fatalf("turn 3 should retry as switch from codex: switched=%v from=%s", result3.Switched, result3.From)
	}
	if strings.Contains(string(out3), "resp_previous_provider") || strings.Contains(string(out3), "session_previous_provider") {
		t.Fatalf("turn 3 leaked foreign state on retry: %s", string(out3))
	}
	_ = out2
}
