package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func TestResponsesClaudePreservesEasyInputMessages(t *testing.T) {
	got := translateResponsesToClaudeForToolPairingTest(t, []byte(`{"model":"claude-sonnet-5-5","instructions":"base","input":[{"role":"developer","content":"policy"},{"role":"user","content":"question"},{"role":"assistant","content":[{"type":"output_text","text":"answer"}]}]}`))
	if got["system"] != "base\n\npolicy" {
		t.Fatalf("system = %#v", got["system"])
	}
	messages := got["messages"].([]any)
	if len(messages) != 2 {
		t.Fatalf("messages = %#v", messages)
	}
	assertClaudeMessageText(t, messages[0], "user", "question")
	assertClaudeMessageText(t, messages[1], "assistant", "answer")
}

func TestResponsesClaudePreservesToolChoice(t *testing.T) {
	for _, tc := range []struct {
		choice any
		want   map[string]any
	}{
		{"auto", map[string]any{"type": "auto"}},
		{"none", map[string]any{"type": "none"}},
		{"required", map[string]any{"type": "any"}},
		{map[string]any{"type": "function", "name": "lookup"}, map[string]any{"type": "tool", "name": "mcp_lookup"}},
	} {
		t.Run(strings.ReplaceAll(tc.want["type"].(string), " ", "_"), func(t *testing.T) {
			body, _ := json.Marshal(map[string]any{"model": "claude-sonnet-5-5", "input": "question", "tools": []any{map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}}}, "tool_choice": tc.choice, "parallel_tool_calls": false})
			got := translateResponsesToClaudeForToolPairingTest(t, body)
			choice := got["tool_choice"].(map[string]any)
			for key, want := range tc.want {
				if choice[key] != want {
					t.Fatalf("choice %s = %#v, want %#v", key, choice[key], want)
				}
			}
			if tc.want["type"] != "none" && choice["disable_parallel_tool_use"] != true {
				t.Fatalf("parallel tool restriction lost: %#v", choice)
			}
		})
	}
}

func TestResponsesClaudeRejectsUnsupportedOrInvalidToolContracts(t *testing.T) {
	for _, fields := range []string{
		`"tools":[{"type":"custom","name":"apply_patch"}]`,
		`"tools":[{"type":"web_search"}]`,
		`"tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}}],"tool_choice":{"type":"function","name":"missing"}`,
		`"tool_choice":"required"`,
		`"tool_choice":"invalid"`,
		`"tool_choice":{"type":"custom","name":"apply_patch"}`,
		`"parallel_tool_calls":"false"`,
		`"input":[{"type":"function_call","call_id":"call","name":"lookup","arguments":"invalid"}]`,
		`"input":[{"type":"function_call","call_id":"call","name":"lookup","arguments":"[]"}]`,
		`"input":[{"type":"custom_tool_call","call_id":"call","name":"apply_patch","input":"patch"}]`,
		`"input":[{"role":"developer","content":[{"type":"input_file","file_id":"synthetic"}]}]`,
		`"input":[{"role":"user","content":123}]`,
		`"input":[{"role":"user","content":[{"type":"input_text","text":false}]}]`,
		`"input":[{"type":"function_call","call_id":"call","name":"lookup","arguments":"{}"}]`,
		`"input":[{"type":"function_call_output","call_id":"missing","output":"orphan"}]`,
		`"input":[{"type":"function_call","call_id":"call","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"call","output":"one"},{"type":"function_call_output","call_id":"call","output":"two"}]`,
	} {
		t.Run(fields, func(t *testing.T) {
			if _, err := translateResponsesToClaudeRequest([]byte(`{"model":"claude-sonnet-5-5",` + fields + `}`)); err == nil {
				t.Fatal("unsupported or invalid contract was silently accepted")
			}
		})
	}
}
