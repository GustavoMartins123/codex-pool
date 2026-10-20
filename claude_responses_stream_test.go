package main

import (
	"bytes"
	"encoding/json"
	"fmt"
	"strings"
	"testing"
)

func TestClaudeResponsesStreamPreservesItemsAndTermination(t *testing.T) {
	for _, stop := range []string{"end_turn", "max_tokens"} {
		t.Run(stop, func(t *testing.T) {
			var wire strings.Builder
			add := func(event map[string]any) { data, _ := json.Marshal(event); fmt.Fprintf(&wire, "data: %s\n\n", data) }
			add(map[string]any{"type": "message_start", "message": map[string]any{"id": "synthetic", "model": "claude-sonnet-5-5"}})
			text := func(index int, value string) {
				add(map[string]any{"type": "content_block_start", "index": index, "content_block": map[string]any{"type": "text", "text": ""}})
				add(map[string]any{"type": "content_block_delta", "index": index, "delta": map[string]any{"type": "text_delta", "text": value}})
				add(map[string]any{"type": "content_block_stop", "index": index})
			}
			text(0, "first")
			text(1, "second")
			add(map[string]any{"type": "content_block_start", "index": 2, "content_block": map[string]any{"type": "tool_use", "id": "call_synthetic", "name": "mcp_lookup", "input": map[string]any{}}})
			add(map[string]any{"type": "content_block_delta", "index": 2, "delta": map[string]any{"type": "input_json_delta", "partial_json": `{"value":7}`}})
			add(map[string]any{"type": "content_block_stop", "index": 2})
			text(3, "last")
			add(map[string]any{"type": "message_delta", "delta": map[string]any{"stop_reason": stop}})
			add(map[string]any{"type": "message_stop"})
			add(map[string]any{"type": "message_stop"}) // A duplicate terminal must not duplicate output.
			var out bytes.Buffer
			writer := &claudeToResponsesWriter{w: &out}
			data := []byte(wire.String())
			for len(data) > 0 {
				n := min(len(data), 7)
				if _, err := writer.Write(data[:n]); err != nil {
					t.Fatal(err)
				}
				data = data[n:]
			}
			events := decodeClaudeResponsesEvents(t, out.String())
			ids := map[int]string{}
			terminals, parts := 0, 0
			for sequence, event := range events {
				if event["sequence_number"] != float64(sequence) {
					t.Fatalf("invalid event sequence: %#v", event)
				}
				typ := event["type"].(string)
				index, _ := event["output_index"].(float64)
				if item, ok := event["item"].(map[string]any); ok {
					id, _ := item["id"].(string)
					if id == "" {
						t.Fatalf("missing item id: %#v", item)
					}
					if typ == "response.output_item.added" {
						ids[int(index)] = id
					} else if ids[int(index)] != id {
						t.Fatalf("item id changed: %#v", event)
					}
				}
				if strings.HasPrefix(typ, "response.function_call_arguments.") || strings.HasPrefix(typ, "response.output_text.") || strings.HasPrefix(typ, "response.content_part.") {
					if event["item_id"] != ids[int(index)] {
						t.Fatalf("event has wrong item id: %#v", event)
					}
				}
				if typ == "response.content_part.done" {
					parts++
				}
				if typ != "response.completed" && typ != "response.incomplete" {
					continue
				}
				terminals++
				response := event["response"].(map[string]any)
				output := response["output"].([]any)
				if len(output) != 3 {
					t.Fatalf("terminal lost output: %#v", output)
				}
				first := output[0].(map[string]any)["content"].([]any)
				if len(first) != 2 || first[0].(map[string]any)["text"] != "first" || first[1].(map[string]any)["text"] != "second" {
					t.Fatalf("text parts lost: %#v", first)
				}
				if output[1].(map[string]any)["arguments"] != `{"value":7}` {
					t.Fatalf("tool arguments lost: %#v", output[1])
				}
				if stop == "max_tokens" {
					if typ != "response.incomplete" || response["status"] != "incomplete" || response["incomplete_details"].(map[string]any)["reason"] != "max_output_tokens" {
						t.Fatalf("truncation reported incorrectly: %#v", event)
					}
				} else if typ != "response.completed" {
					t.Fatal("completed response was marked incomplete")
				}
			}
			if terminals != 1 || parts != 3 {
				t.Fatalf("terminals = %d, completed parts = %d", terminals, parts)
			}
		})
	}
}

func TestClaudeResponsesStreamDoesNotConvertProviderErrorToSuccess(t *testing.T) {
	var out bytes.Buffer
	writer := &claudeToResponsesWriter{w: &out}
	writer.Write([]byte("data: {\"type\":\"error\",\"error\":{\"message\":\"private upstream details\"}}\n\ndata: {\"type\":\"message_stop\"}\n\n"))
	events := decodeClaudeResponsesEvents(t, out.String())
	if len(events) != 1 || events[0]["type"] != "response.failed" || strings.Contains(out.String(), "private upstream details") {
		t.Fatalf("invalid error stream: %s", out.String())
	}
}

func decodeClaudeResponsesEvents(t *testing.T, wire string) []map[string]any {
	t.Helper()
	var result []map[string]any
	for _, line := range strings.Split(wire, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event map[string]any
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatal(err)
		}
		result = append(result, event)
	}
	return result
}
