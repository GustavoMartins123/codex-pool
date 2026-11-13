package main

import (
	"encoding/json"
	"strings"
	"testing"
)

func qwenCodexToolDeclarations() []any {
	return []any{
		map[string]any{"type": "namespace", "name": "functions", "tools": []any{
			map[string]any{"type": "function", "name": "exec_command", "description": "Runs a command.", "parameters": map[string]any{"type": "object", "properties": map[string]any{"cmd": map[string]any{"type": "string"}}, "required": []any{"cmd"}}},
		}},
		map[string]any{"type": "custom", "name": "apply_patch", "description": "Edits files.", "format": map[string]any{"type": "grammar", "syntax": "lark", "definition": "start: begin_patch"}},
	}
}

func qwenPreparedTestRequest(t *testing.T, payload map[string]any) *qwenResponsesRequest {
	t.Helper()
	body, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := prepareQwenResponsesRequest("/v1/responses", body, "qwen3.8-max")
	if err != nil {
		t.Fatal(err)
	}
	return prepared
}

func TestQwenResponsesConvertsCodexToolsHistoryAndRawPatch(t *testing.T) {
	patch := "*** Begin Patch\n*** Add File: café.txt\n+Olá\n*** End Patch"
	payload := map[string]any{
		"model": "qwen3.8-max", "stream": true, "instructions": "Use the tools.", "parallel_tool_calls": false,
		"previous_response_id": "resp-prior", "store": false, "max_output_tokens": 4096,
		"tool_choice": map[string]any{"type": "function", "name": "exec_command", "namespace": "functions"},
		"input": []any{
			map[string]any{"type": "additional_tools", "tools": qwenCodexToolDeclarations()},
			map[string]any{"role": "developer", "content": []any{map[string]any{"type": "input_text", "text": "Keep the patch exact."}}},
			map[string]any{"role": "user", "content": "Create a file"},
			map[string]any{"type": "reasoning", "summary": []any{map[string]any{"type": "summary_text", "text": "Check the directory first."}}},
			map[string]any{"type": "function_call", "call_id": "call-command", "namespace": "functions", "name": "exec_command", "arguments": `{"cmd":"pwd"}`},
			map[string]any{"type": "function_call_output", "call_id": "call-command", "output": []any{map[string]any{"type": "input_text", "text": "/workspace"}}},
			map[string]any{"type": "custom_tool_call", "call_id": "call-patch", "name": "apply_patch", "input": patch},
			map[string]any{"type": "custom_tool_call_output", "call_id": "call-patch", "output": "Done!"},
		},
	}
	prepared := qwenPreparedTestRequest(t, payload)
	var chat map[string]any
	json.Unmarshal(prepared.Body, &chat)
	for _, key := range []string{"input", "instructions", "previous_response_id", "store", "reasoning"} {
		if _, exists := chat[key]; exists {
			t.Fatalf("Responses-only field %q reached Qwen", key)
		}
	}
	tools := anySlice(chat["tools"])
	if len(tools) != 2 || mapValue(chat["stream_options"])["include_usage"] != true || chat["max_tokens"] != float64(4096) || chat["parallel_tool_calls"] != false {
		t.Fatalf("missing tool or generation contract: %s", prepared.Body)
	}
	execName := prepared.toolNames[qwenResponseToolKey("functions", "exec_command")]
	if !qwenFunctionNamePattern.MatchString(execName) || mapValue(mapValue(chat["tool_choice"])["function"])["name"] != execName {
		t.Fatal("namespaced tool choice was not mapped to the upstream declaration")
	}
	if !strings.Contains(stringValue(mapValue(mapValue(tools[1])["function"])["description"]), "start: begin_patch") {
		t.Fatal("custom tool grammar was not provided to Qwen")
	}
	messages := anySlice(chat["messages"])
	if len(messages) != 7 || mapValue(messages[0])["content"] != "Use the tools." || mapValue(messages[1])["role"] != "system" {
		t.Fatalf("system messages or tool history were lost: %s", prepared.Body)
	}
	command := mapValue(messages[3])
	if command["reasoning_content"] != "Check the directory first." || mapValue(mapValue(anySlice(command["tool_calls"])[0])["function"])["name"] != execName || mapValue(messages[4])["tool_call_id"] != "call-command" {
		t.Fatal("namespaced call history or reasoning was lost")
	}
	var args map[string]string
	if json.Unmarshal([]byte(stringValue(mapValue(mapValue(anySlice(mapValue(messages[5])["tool_calls"])[0])["function"])["arguments"])), &args) != nil || args["input"] != patch || mapValue(messages[6])["content"] != "Done!" {
		t.Fatal("raw custom tool input or output was changed")
	}
}

func TestQwenResponsesRejectsUnsupportedAndBrokenToolContracts(t *testing.T) {
	for _, test := range []struct {
		name string
		body string
	}{
		{"orphan output", `{"input":[{"type":"function_call_output","call_id":"missing","output":"done"}]}`},
		{"missing output", `{"input":[{"type":"function_call","call_id":"c","name":"lookup","arguments":"{}"}]}`},
		{"duplicate output", `{"input":[{"type":"function_call","call_id":"c","name":"lookup","arguments":"{}"},{"type":"function_call_output","call_id":"c","output":"ok"},{"type":"function_call_output","call_id":"c","output":"again"}]}`},
		{"malformed arguments", `{"input":[{"type":"function_call","call_id":"c","name":"lookup","arguments":"{"}]}`},
		{"hosted tool", `{"input":"hi","tools":[{"type":"web_search"}]}`},
		{"missing schema", `{"input":"hi","tools":[{"type":"function","name":"lookup"}]}`},
		{"duplicate tool", `{"input":"hi","tools":[{"type":"function","name":"lookup","parameters":{"type":"object"}},{"type":"function","name":"lookup","parameters":{"type":"object"}}]}`},
		{"required unsupported", `{"input":"hi","tool_choice":"required"}`},
		{"undeclared choice", `{"input":"hi","tool_choice":{"type":"function","name":"missing"}}`},
		{"invalid parallel", `{"input":"hi","parallel_tool_calls":"true"}`},
		{"invalid stream", `{"input":"hi","stream":"true"}`},
		{"item reference", `{"input":[{"type":"item_reference","id":"item"}]}`},
		{"missing input", `{"previous_response_id":"resp-old"}`},
	} {
		t.Run(test.name, func(t *testing.T) {
			if _, err := prepareQwenResponsesRequest("/v1/responses", []byte(test.body), "qwen3.8-max"); err == nil {
				t.Fatalf("invalid request was accepted: %s", test.body)
			}
		})
	}
	if _, err := prepareQwenResponsesRequest("/v1/responses/compact", []byte(`{"input":"hi"}`), "qwen3.8-max"); err == nil {
		t.Fatal("unsupported compaction endpoint was accepted")
	}
}

func TestQwenResponsesPreservesImagesAndAvoidsToolNameCollisions(t *testing.T) {
	prepared := qwenPreparedTestRequest(t, map[string]any{"tools": []any{
		map[string]any{"type": "namespace", "name": "first", "tools": []any{map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}}}},
		map[string]any{"type": "namespace", "name": "second", "tools": []any{map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}}}},
	}, "input": []any{map[string]any{"role": "user", "content": []any{map[string]any{"type": "input_image", "image_url": "data:image/png;base64,dGVzdA==", "detail": "high"}, map[string]any{"type": "input_text", "text": "Describe it"}}}}})
	first := prepared.toolNames[qwenResponseToolKey("first", "lookup")]
	second := prepared.toolNames[qwenResponseToolKey("second", "lookup")]
	if first == second || !qwenFunctionNamePattern.MatchString(first) || !qwenFunctionNamePattern.MatchString(second) {
		t.Fatal("namespaced tools collided or produced invalid Qwen function names")
	}
	var request map[string]any
	json.Unmarshal(prepared.Body, &request)
	image := mapValue(anySlice(mapValue(anySlice(request["messages"])[0])["content"])[0])
	if image["type"] != "image_url" || mapValue(image["image_url"])["url"] != "data:image/png;base64,dGVzdA==" || mapValue(image["image_url"])["detail"] != "high" {
		t.Fatal("image input was lost in Responses conversion")
	}
}

func TestQwenResponsesDropsOtherProvidersEncryptedReasoning(t *testing.T) {
	// A conversation that started on a GPT model carries encrypted reasoning
	// items; switching to Qwen must keep the visible history.
	body := `{"input":[
		{"type":"message","role":"user","content":[{"type":"input_text","text":"plan the work"}]},
		{"type":"reasoning","summary":[],"encrypted_content":"gAAAAopaque-openai-ciphertext"},
		{"type":"message","role":"assistant","content":[{"type":"output_text","text":"Here is the plan."}]},
		{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}
	]}`
	prepared, err := prepareQwenResponsesRequest("/v1/responses", []byte(body), "qwen4-max-preview")
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(prepared.Body, &request); err != nil {
		t.Fatal(err)
	}
	encoded := string(prepared.Body)
	if strings.Contains(encoded, "ciphertext") || strings.Contains(encoded, "reasoning_content") {
		t.Fatalf("encrypted reasoning leaked to Qwen: %s", encoded)
	}
	var roles []string
	for _, raw := range anySlice(request["messages"]) {
		roles = append(roles, stringValue(mapValue(raw)["role"]))
	}
	if strings.Join(roles, ",") != "user,assistant,user" || !strings.Contains(encoded, "Here is the plan.") {
		t.Fatalf("visible history was not kept: %s", encoded)
	}
}
