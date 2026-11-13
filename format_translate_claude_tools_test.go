package main

import (
	"encoding/json"
	"testing"
)

func TestClaudeToolResultKeepsAttachedText(t *testing.T) {
	body := `{"model":"qwen3.8-max","messages":[
		{"role":"user","content":"run it"},
		{"role":"assistant","content":[{"type":"tool_use","id":"call-a","name":"Bash","input":{"command":"ls"}}]},
		{"role":"user","content":[
			{"type":"tool_result","tool_use_id":"call-a","content":"a.txt"},
			{"type":"text","text":"<system-reminder>file changed</system-reminder>"}
		]}]}`
	translated, err := translateClaudeReqToOpenAI([]byte(body))
	if err != nil {
		t.Fatal(err)
	}
	var request map[string]any
	if err := json.Unmarshal(translated, &request); err != nil {
		t.Fatal(err)
	}
	messages := anySlice(request["messages"])
	if len(messages) != 4 {
		t.Fatalf("messages = %v", messages)
	}
	tool, reminder := mapValue(messages[2]), mapValue(messages[3])
	if tool["role"] != "tool" || tool["tool_call_id"] != "call-a" || tool["content"] != "a.txt" {
		t.Fatalf("tool result = %v", tool)
	}
	if reminder["role"] != "user" || reminder["content"] != "<system-reminder>file changed</system-reminder>" {
		t.Fatalf("text next to the tool result was lost: %v", reminder)
	}
}
