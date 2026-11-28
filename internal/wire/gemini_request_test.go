package wire

import (
	"testing"

	"codex-pool-proxy/internal/jsonvalue"
)

func TestAntigravityMergeConsecutiveRoleContents(t *testing.T) {
	contents := []any{
		map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hello"}}},
		map[string]any{"role": "model", "parts": []any{map[string]any{"text": "running tool"}}},
		map[string]any{"role": "model", "parts": []any{map[string]any{"functionCall": map[string]any{"name": "exec"}}}},
		map[string]any{"role": "user", "parts": []any{map[string]any{"functionResponse": map[string]any{"name": "exec", "response": map[string]any{"result": "ok"}}}}},
		map[string]any{"role": "user", "parts": []any{map[string]any{"text": "next question"}}},
	}

	merged := antigravityMergeConsecutiveRoleContents(contents)
	if len(merged) != 3 {
		t.Fatalf("expected 3 merged contents, got %d", len(merged))
	}
	turn0 := jsonvalue.Map(merged[0])
	if jsonvalue.String(turn0["role"]) != "user" || len(jsonvalue.Slice(turn0["parts"])) != 1 {
		t.Fatalf("turn 0 mismatch: %#v", turn0)
	}
	turn1 := jsonvalue.Map(merged[1])
	if jsonvalue.String(turn1["role"]) != "model" || len(jsonvalue.Slice(turn1["parts"])) != 2 {
		t.Fatalf("turn 1 mismatch: %#v", turn1)
	}
	turn2 := jsonvalue.Map(merged[2])
	if jsonvalue.String(turn2["role"]) != "user" || len(jsonvalue.Slice(turn2["parts"])) != 2 {
		t.Fatalf("turn 2 mismatch: %#v", turn2)
	}
}

func TestAntigravityResponsesScopesRepeatedToolCallIDsByTurn(t *testing.T) {
	request := map[string]any{
		"model": "antigravity/gemini-3.8-flash-high",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "run twice"}}},
			map[string]any{"type": "function_call", "call_id": "call-reused", "name": "write_stdin", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "call-reused", "output": "first"},
			map[string]any{"type": "function_call", "call_id": "call-reused", "name": "exec_command", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "call-reused", "output": "second"},
		},
	}
	prepared, err := antigravityResponsesToGemini(request, "gemini-3.8-flash-high")
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, raw := range prepared["contents"].([]any) {
		for _, rawPart := range jsonvalue.Slice(jsonvalue.Map(raw)["parts"]) {
			if response := jsonvalue.Map(jsonvalue.Map(rawPart)["functionResponse"]); response != nil {
				names = append(names, jsonvalue.String(response["name"]))
			}
		}
	}
	if len(names) != 2 || names[0] != "write_stdin" || names[1] != "exec_command" {
		t.Fatalf("repeated call ID names = %#v", names)
	}
}

func TestAntigravityResponsesAlignsDetachedParallelToolResponses(t *testing.T) {
	request := map[string]any{
		"model": "antigravity/gemini-3.8-flash-high",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "run both"}}},
			map[string]any{"type": "function_call", "call_id": "call-1", "name": "first", "arguments": "{}"},
			map[string]any{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "plan"}}},
			map[string]any{"type": "function_call", "call_id": "call-2", "name": "second", "arguments": "{}"},
			map[string]any{"type": "function_call_output", "call_id": "call-2", "output": "two"},
			map[string]any{"type": "function_call_output", "call_id": "call-1", "output": "one"},
		},
	}
	prepared, err := antigravityResponsesToGemini(request, "gemini-3.8-flash-high")
	if err != nil {
		t.Fatal(err)
	}
	contents := prepared["contents"].([]any)
	var responses []any
	for _, raw := range contents {
		for _, rawPart := range jsonvalue.Slice(jsonvalue.Map(raw)["parts"]) {
			if response := jsonvalue.Map(jsonvalue.Map(rawPart)["functionResponse"]); response != nil {
				responses = append(responses, response)
			}
		}
	}
	if len(responses) != 2 || jsonvalue.String(responses[0].(map[string]any)["id"]) != "call-1" || jsonvalue.String(responses[1].(map[string]any)["id"]) != "call-2" {
		t.Fatalf("function response order = %#v", responses)
	}
}

func TestAntigravityResponsesMergesConsecutiveRolesInMultiTurnHistory(t *testing.T) {
	req := map[string]any{
		"model": "antigravity/gemini-3.8-flash-high",
		"input": []any{
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "start"}}},
			map[string]any{"type": "message", "role": "assistant", "content": []any{map[string]any{"type": "output_text", "text": "checking..."}}},
			map[string]any{"type": "function_call", "call_id": "call-1", "name": "exec_command", "arguments": "{\"cmd\":\"pwd\"}"},
			map[string]any{"type": "function_call_output", "call_id": "call-1", "output": "/home"},
			map[string]any{"type": "message", "role": "user", "content": []any{map[string]any{"type": "input_text", "text": "continue"}}},
		},
	}
	geminiReq, err := antigravityResponsesToGemini(req, "gemini-3.8-flash-high")
	if err != nil {
		t.Fatal(err)
	}
	contents, _ := geminiReq["contents"].([]any)
	if len(contents) != 3 {
		t.Fatalf("contents length = %d, want 3", len(contents))
	}
	lastRole := ""
	for i, c := range contents {
		m := jsonvalue.Map(c)
		role := jsonvalue.String(m["role"])
		if role == lastRole {
			t.Fatalf("role did not alternate at index %d: %s", i, role)
		}
		lastRole = role
	}
}

func TestCleanAntigravitySchemaDropsNullRequired(t *testing.T) {
	got := cleanAntigravitySchema(map[string]any{
		"type": "object",
		"properties": map[string]any{
			"nested": map[string]any{"type": "object", "required": nil},
		},
		"required": nil,
	})
	if _, ok := got["required"]; ok {
		t.Fatalf("top-level null required survived: %#v", got)
	}
	nested := got["properties"].(map[string]any)["nested"].(map[string]any)
	if _, ok := nested["required"]; ok {
		t.Fatalf("nested null required survived: %#v", nested)
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
	thinking := jsonvalue.Map(jsonvalue.Map(converted["generationConfig"])["thinkingConfig"])
	if thinking["thinkingLevel"] != "high" {
		t.Fatalf("effort was lost: %+v", thinking)
	}
	if _, invented := thinking["thinkingBudget"]; invented {
		t.Fatal("effort was replaced with an invented token budget")
	}
}

func TestClientRequestToGeminiRejectsUnknownFormat(t *testing.T) {
	_, _, err := ClientRequestToGemini(map[string]any{"contents": []any{}}, FormatUnknown, "gemini-3.8-flash-high")
	if err == nil || err.Error() != `unsupported gemini client format "unknown"` {
		t.Fatalf("err = %v", err)
	}
}

func TestClientRequestToGeminiCanonicalizesSnakeCaseSystemInstruction(t *testing.T) {
	root := map[string]any{
		"system_instruction": map[string]any{"parts": []any{map[string]any{"text": "be terse"}}},
		"contents":           []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hi"}}}},
	}
	gemini, _, err := ClientRequestToGemini(root, FormatGemini, "gemini-3.8-flash-high")
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := gemini["system_instruction"]; ok {
		t.Fatal("snake_case system_instruction was forwarded")
	}
	parts := jsonvalue.Slice(jsonvalue.Map(gemini["systemInstruction"])["parts"])
	if len(parts) != 1 || jsonvalue.String(jsonvalue.Map(parts[0])["text"]) != "be terse" {
		t.Fatalf("systemInstruction = %#v", gemini["systemInstruction"])
	}
	if _, ok := root["system_instruction"]; !ok {
		t.Fatal("client request was mutated")
	}
}

func TestClientRequestToGeminiKeepsBothSystemSpellingsAsSent(t *testing.T) {
	root := map[string]any{
		"systemInstruction":  map[string]any{"parts": []any{map[string]any{"text": "camel"}}},
		"system_instruction": map[string]any{"parts": []any{map[string]any{"text": "snake"}}},
		"contents":           []any{map[string]any{"role": "user", "parts": []any{map[string]any{"text": "hi"}}}},
	}
	gemini, _, err := ClientRequestToGemini(root, FormatGemini, "gemini-3.8-flash-high")
	if err != nil {
		t.Fatal(err)
	}
	_, camel := gemini["systemInstruction"]
	_, snake := gemini["system_instruction"]
	if !camel || !snake {
		t.Fatalf("both spellings should pass through unchanged: camel=%v snake=%v", camel, snake)
	}
}

func TestCleanAntigravitySchemaOrdersHintsStably(t *testing.T) {
	const want = "No extra properties allowed (format: date) (minLength: 2) (Accepts: string | number) ((nullable))"
	for range 50 {
		got := cleanAntigravitySchema(map[string]any{
			"type":                 []any{"string", "number", "null"},
			"minLength":            2,
			"format":               "date",
			"additionalProperties": false,
		})
		if got["description"] != want {
			t.Fatalf("description = %q, want %q", got["description"], want)
		}
	}
}

func TestAntigravityGenerationConfigPrefersMostSpecificTokenLimit(t *testing.T) {
	for _, tc := range []struct {
		input map[string]any
		want  any
	}{
		{map[string]any{"max_tokens": 1.0}, 1.0},
		{map[string]any{"max_tokens": 1.0, "max_completion_tokens": 2.0}, 2.0},
		{map[string]any{"max_tokens": 1.0, "max_output_tokens": 3.0}, 3.0},
		{map[string]any{"max_tokens": 1.0, "max_completion_tokens": 2.0, "max_output_tokens": 3.0}, 3.0},
	} {
		for range 20 {
			result := map[string]any{}
			antigravityCopyGenerationConfig(tc.input, result)
			if got := jsonvalue.Map(result["generationConfig"])["maxOutputTokens"]; got != tc.want {
				t.Fatalf("%v: maxOutputTokens = %v, want %v", tc.input, got, tc.want)
			}
		}
	}
}
