package main

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func claudeStreamEvents(t *testing.T, body string) []map[string]any {
	t.Helper()
	var events []map[string]any
	for _, frame := range strings.Split(body, "\n\n") {
		kind, data := parseSSEEvent([]byte(frame))
		if len(data) == 0 {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal(data, &event); err != nil || event["type"] != kind {
			t.Fatalf("invalid Claude event %q: %s", kind, data)
		}
		events = append(events, event)
	}
	return events
}

func TestOpenAIStreamToClaudeReportsPromptUsage(t *testing.T) {
	var out bytes.Buffer
	w := &sseTranslateWriter{w: &out, direction: TranslateOAIToClaude}
	for _, chunk := range []string{
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":"hi"},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":30}}}`,
		`[DONE]`,
	} {
		w.Write([]byte("data: " + chunk + "\n\n"))
	}
	events := claudeStreamEvents(t, out.String())
	usage := mapValue(events[len(events)-2]["usage"])
	if usage["input_tokens"] != float64(70) || usage["cache_read_input_tokens"] != float64(30) || usage["output_tokens"] != float64(20) {
		t.Fatalf("message_delta usage = %v\n%s", usage, out.String())
	}
}

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

func TestOpenAIResponseToClaudeSplitsCachedInput(t *testing.T) {
	translated, err := translateOpenAIRespToClaude([]byte(`{"id":"chat-1","model":"qwen3.8-max","choices":[{"message":{"role":"assistant","content":"hi"},"finish_reason":"stop"}],"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":30}}}`), "qwen3.8-max")
	if err != nil {
		t.Fatal(err)
	}
	var response map[string]any
	if err := json.Unmarshal(translated, &response); err != nil {
		t.Fatal(err)
	}
	usage := mapValue(response["usage"])
	if usage["input_tokens"] != float64(70) || usage["cache_read_input_tokens"] != float64(30) || usage["output_tokens"] != float64(20) {
		t.Fatalf("usage = %v", usage)
	}
}
