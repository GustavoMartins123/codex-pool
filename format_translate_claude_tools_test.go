package main

import (
	"bytes"
	"encoding/json"
	"fmt"
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

func TestOpenAIStreamToClaudeParallelToolCalls(t *testing.T) {
	var out bytes.Buffer
	w := &sseTranslateWriter{w: &out, direction: TranslateOAIToClaude}
	chunks := []string{
		// No role chunk: the first delta must still open the message.
		`{"id":"chat-1","model":"qwen3.8-max","choices":[{"index":0,"delta":{"content":"Checking."}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-a","type":"function","function":{"name":"Bash","arguments":"{\"command\":"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"id":"call-b","type":"function","function":{"name":"Read","arguments":"{\"file_path\":"}}]}}]}`,
		// Interleaved fragments that repeat the tool name must not open new blocks.
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"function":{"name":"Bash","arguments":"\"ls\"}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":1,"function":{"arguments":"\"a.txt\"}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{},"finish_reason":"stop"}]}`,
		`{"choices":[],"usage":{"prompt_tokens":100,"completion_tokens":20,"prompt_tokens_details":{"cached_tokens":30}}}`,
		`[DONE]`,
	}
	for _, chunk := range chunks {
		if _, err := w.Write([]byte("data: " + chunk + "\n\n")); err != nil {
			t.Fatal(err)
		}
	}
	events := claudeStreamEvents(t, out.String())
	if events[0]["type"] != "message_start" || events[len(events)-1]["type"] != "message_stop" {
		t.Fatalf("message envelope is incomplete: %s", out.String())
	}
	open := map[float64]bool{}
	blocks := map[float64]map[string]any{}
	inputs := map[float64]string{}
	var delta map[string]any
	for _, event := range events {
		index, _ := event["index"].(float64)
		switch event["type"] {
		case "content_block_start":
			if len(open) != 0 {
				t.Fatalf("block %v started while another block is open", index)
			}
			open[index] = true
			blocks[index] = mapValue(event["content_block"])
		case "content_block_stop":
			if !open[index] {
				t.Fatalf("block %v stopped without being open", index)
			}
			delete(open, index)
		case "content_block_delta":
			if part := mapValue(event["delta"]); part["type"] == "input_json_delta" {
				inputs[index] += stringValue(part["partial_json"])
			}
		case "message_delta":
			delta = event
		}
	}
	if len(open) != 0 || len(blocks) != 3 || blocks[0]["type"] != "text" {
		t.Fatalf("unexpected Claude content blocks: %v", blocks)
	}
	for index, want := range map[float64][3]string{1: {"call-a", "Bash", `{"command":"ls"}`}, 2: {"call-b", "Read", `{"file_path":"a.txt"}`}} {
		block := blocks[index]
		if block["type"] != "tool_use" || block["id"] != want[0] || block["name"] != want[1] || inputs[index] != want[2] {
			t.Errorf("tool block %v = %v input %q, want %v", index, block, inputs[index], want)
		}
	}
	usage := mapValue(delta["usage"])
	if mapValue(delta["delta"])["stop_reason"] != "tool_use" || usage["input_tokens"] != float64(70) || usage["cache_read_input_tokens"] != float64(30) || usage["output_tokens"] != float64(20) {
		t.Fatalf("final message_delta = %v", delta)
	}
}

func TestOpenAIStreamToClaudeTextAfterToolStartsNewBlock(t *testing.T) {
	var out bytes.Buffer
	w := &sseTranslateWriter{w: &out, direction: TranslateOAIToClaude}
	for _, chunk := range []string{
		`{"choices":[{"index":0,"delta":{"role":"assistant","content":"Before"}}]}`,
		`{"choices":[{"index":0,"delta":{"tool_calls":[{"index":0,"id":"call-a","function":{"name":"Bash","arguments":"{}"}}]}}]}`,
		`{"choices":[{"index":0,"delta":{"content":"After"}}]}`,
		`[DONE]`,
	} {
		w.Write([]byte("data: " + chunk + "\n\n"))
	}
	var texts []string
	for _, event := range claudeStreamEvents(t, out.String()) {
		if part := mapValue(event["delta"]); part["type"] == "text_delta" {
			texts = append(texts, fmt.Sprintf("%s@%v", part["text"], event["index"]))
		}
	}
	if strings.Join(texts, ",") != "Before@0,After@2" {
		t.Fatalf("text deltas landed in the wrong blocks: %v\n%s", texts, out.String())
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
