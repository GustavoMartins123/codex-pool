package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

// Codex 0.155+ sends tools through `additional_tools` input items with nested
// namespaces and freeform custom tools instead of the top-level `tools` array.
// The Antigravity translator must flatten them into upstream function
// declarations and restore the namespace / custom_tool_call shape on the way
// back, otherwise the upstream sees no tools and the model ends its turn with
// reasoning only.
func TestAntigravityAdditionalToolsFlattenNamespaces(t *testing.T) {
	body := []byte(`{
		"model":"antigravity/gemini-3.8-flash-high",
		"input":[
			{"type":"additional_tools","id":"at_1","role":"developer","tools":[
				{"type":"namespace","name":"functions","tools":[
					{"type":"function","name":"exec_command","description":"Runs a command.","strict":false,
					 "parameters":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"],"additionalProperties":false}}
				]},
				{"type":"namespace","name":"mcp__node_repl","tools":[
					{"type":"function","name":"js","description":"Execute JavaScript.","parameters":{"type":"object","properties":{"code":{"type":"string"}},"required":["code"]}}
				]},
				{"type":"custom","name":"apply_patch","description":"The apply_patch tool edits files.",
				 "format":{"type":"grammar","syntax":"lark","definition":"start: begin_patch"}}
			]},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"list the files"}]}
		],
		"tools":[],
		"reasoning":{"effort":"high","context":"all_turns"},
		"store":false,
		"stream":true
	}`)
	prepared, err := prepareAntigravityRequest("/v1/responses", body, "antigravity/gemini-3.8-flash-high", "project-1", "")
	if err != nil {
		t.Fatal(err)
	}
	envelope := decodeMap(t, prepared.Body)
	inner := envelope["request"].(map[string]any)
	declarations := inner["tools"].([]any)[0].(map[string]any)["functionDeclarations"].([]any)
	names := map[string]bool{}
	for _, raw := range declarations {
		name := raw.(map[string]any)["name"].(string)
		names[name] = true
	}
	for _, want := range []string{"functions.exec_command", "mcp__node_repl.js", "apply_patch"} {
		if !names[want] {
			t.Fatalf("missing upstream declaration %q in %#v", want, names)
		}
	}
	if !prepared.ResponsesNamespacedTools["functions.exec_command"] || !prepared.ResponsesNamespacedTools["mcp__node_repl.js"] {
		t.Fatalf("namespaced set = %#v", prepared.ResponsesNamespacedTools)
	}
	if prepared.ResponsesNamespacedTools["apply_patch"] {
		t.Fatal("root-level custom tool must not be marked namespaced")
	}
	if !prepared.ResponsesCustomTools["apply_patch"] {
		t.Fatalf("custom set = %#v", prepared.ResponsesCustomTools)
	}
	contents := inner["contents"].([]any)
	for _, raw := range contents {
		entry := raw.(map[string]any)
		for _, partRaw := range entry["parts"].([]any) {
			part := partRaw.(map[string]any)
			if text, _ := part["text"].(string); strings.Contains(text, "additional_tools") {
				t.Fatalf("additional_tools item leaked into contents: %#v", part)
			}
		}
	}

	// Upstream answers with a namespaced function call plus a freeform
	// custom call; the stream writer must restore the client shape.
	response := map[string]any{"candidates": []any{map[string]any{
		"content": map[string]any{"parts": []any{
			map[string]any{"thought": true, "text": "thinking"},
			map[string]any{"functionCall": map[string]any{"name": "functions.exec_command", "id": "call-1", "args": map[string]any{"cmd": "dir"}}},
			map[string]any{"functionCall": map[string]any{"name": "apply_patch", "id": "call-2", "args": map[string]any{"input": "*** Begin Patch"}}},
		}},
		"finishReason": "STOP",
	}}}
	var out bytes.Buffer
	writer := newAntigravityStreamWriter(&out, antigravityFormatResponses, "antigravity/gemini-3.8-flash-high")
	writer.setResponsesFunctionNames(prepared.ResponsesFunctionNames)
	writer.setResponsesToolMetadata(prepared.ResponsesNamespacedTools, prepared.ResponsesCustomTools)
	chunk, err := json.Marshal(map[string]any{"response": response})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.Write([]byte("data: " + string(chunk) + "\n\n")); err != nil {
		t.Fatal(err)
	}
	stream := out.String()
	if !strings.Contains(stream, `"type":"function_call"`) || !strings.Contains(stream, `"name":"exec_command"`) || !strings.Contains(stream, `"namespace":"functions"`) {
		t.Fatalf("namespaced function_call missing from stream:\n%s", stream)
	}
	if !strings.Contains(stream, `"type":"custom_tool_call"`) || !strings.Contains(stream, `"input":"*** Begin Patch"`) {
		t.Fatalf("custom_tool_call missing from stream:\n%s", stream)
	}
}

func TestAntigravityAdditionalToolsReplayNamespacedHistory(t *testing.T) {
	body := []byte(`{
		"model":"antigravity/gemini-3.8-flash-high",
		"input":[
			{"type":"additional_tools","id":"at_1","role":"developer","tools":[
				{"type":"namespace","name":"functions","tools":[
					{"type":"function","name":"exec_command","parameters":{"type":"object","properties":{"cmd":{"type":"string"}},"required":["cmd"]}}
				]},
				{"type":"custom","name":"apply_patch","format":{"type":"grammar","syntax":"lark","definition":"start: begin_patch"}}
			]},
			{"type":"function_call","call_id":"call-1","name":"exec_command","namespace":"functions","arguments":"{\"cmd\":\"dir\"}"},
			{"type":"function_call_output","call_id":"call-1","output":"file-a\nfile-b"},
			{"type":"custom_tool_call","call_id":"call-2","name":"apply_patch","input":"*** Begin Patch"},
			{"type":"custom_tool_call_output","call_id":"call-2","output":"Done!"},
			{"type":"message","role":"user","content":[{"type":"input_text","text":"continue"}]}
		],
		"stream":false
	}`)
	prepared, err := prepareAntigravityRequest("/v1/responses", body, "antigravity/gemini-3.8-flash-high", "project-1", "")
	if err != nil {
		t.Fatal(err)
	}
	envelope := decodeMap(t, prepared.Body)
	inner := envelope["request"].(map[string]any)
	encoded, err := json.Marshal(inner["contents"])
	if err != nil {
		t.Fatal(err)
	}
	contents := string(encoded)
	if !strings.Contains(contents, `"name":"functions.exec_command"`) {
		t.Fatalf("namespaced replay call missing:\n%s", contents)
	}
	if !strings.Contains(contents, `"name":"apply_patch"`) {
		t.Fatalf("custom replay call missing:\n%s", contents)
	}
	for _, callID := range []string{"call-1", "call-2"} {
		if !strings.Contains(contents, `"id":"`+callID+`"`) {
			t.Fatalf("call id %s missing from replay:\n%s", callID, contents)
		}
	}
	// Both outputs must resolve to upstream names so Gemini pairs them.
	if strings.Count(contents, "functionResponse") != 2 {
		t.Fatalf("expected two function responses:\n%s", contents)
	}
}
