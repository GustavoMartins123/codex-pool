package main

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func qwenChatSSE(delta map[string]any, finish any) string {
	encoded, _ := json.Marshal(map[string]any{"id": "chat-test", "choices": []any{map[string]any{"index": 0, "delta": delta, "finish_reason": finish}}})
	return "data: " + string(encoded) + "\n\n"
}

func TestQwenResponsesStreamFailsClosedWithoutExecutingBrokenCalls(t *testing.T) {
	for _, test := range []struct {
		name, stream string
	}{
		{"missing DONE", qwenChatSSE(map[string]any{"content": "partial"}, "stop")},
		{"missing finish", qwenChatSSE(map[string]any{"content": "partial"}, nil) + "data: [DONE]\n\n"},
		{"malformed chunk", "data: {\n\ndata: [DONE]\n\n"},
		{"upstream error", "data: {\"error\":{\"message\":\"synthetic-secret\"}}\n\n"},
		{"unknown tool", qwenChatSSE(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call-x", "function": map[string]any{"name": "missing", "arguments": "{}"}}}}, "tool_calls") + "data: [DONE]\n\n"},
		{"incomplete arguments", qwenChatSSE(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "id": "call-x", "function": map[string]any{"name": "lookup", "arguments": "{"}}}}, "tool_calls") + "data: [DONE]\n\n"},
		{"missing call ID", qwenChatSSE(map[string]any{"tool_calls": []any{map[string]any{"index": 0, "function": map[string]any{"name": "lookup", "arguments": "{}"}}}}, "tool_calls") + "data: [DONE]\n\n"},
		{"missing calls", qwenChatSSE(map[string]any{}, "tool_calls") + "data: [DONE]\n\n"},
	} {
		t.Run(test.name, func(t *testing.T) {
			prepared := qwenPreparedTestRequest(t, map[string]any{"input": "Use lookup", "stream": true, "tools": []any{map[string]any{"type": "function", "name": "lookup", "parameters": map[string]any{"type": "object"}}}})
			response := qwenJSONResponse(200, test.stream)
			response.Header.Set("Content-Type", "text/event-stream")
			rr := httptest.NewRecorder()
			err := writeQwenResponsesResponse(rr, response, prepared, 0, func() {}, nil)
			if err == nil || strings.Contains(rr.Body.String(), "synthetic-secret") {
				t.Fatal("invalid Qwen stream did not fail closed")
			}
			events := qwenResponsesEvents(t, rr.Body.Bytes())
			if len(events) == 0 || events[len(events)-1]["type"] != "response.failed" {
				t.Fatalf("missing explicit terminal failure: %s", rr.Body.String())
			}
			for _, event := range events {
				if event["type"] == "response.completed" || event["type"] == "response.output_item.done" {
					t.Fatal("broken response completed a tool call or reported success")
				}
			}
		})
	}
}

func TestQwenResponsesStreamReportsOutputLimitAndIgnoresRepeatedTerminals(t *testing.T) {
	prepared := qwenPreparedTestRequest(t, map[string]any{"input": "Write", "stream": true})
	response := qwenJSONResponse(200, qwenChatSSE(map[string]any{"content": "partial"}, "length")+"data: [DONE]\n\ndata: [DONE]\n\n")
	response.Header.Set("Content-Type", "text/event-stream")
	rr := httptest.NewRecorder()
	if err := writeQwenResponsesResponse(rr, response, prepared, 0, func() {}, nil); err != nil {
		t.Fatal(err)
	}
	events := qwenResponsesEvents(t, rr.Body.Bytes())
	terminal := events[len(events)-1]
	if terminal["type"] != "response.incomplete" || mapValue(mapValue(terminal["response"])["incomplete_details"])["reason"] != "max_output_tokens" || strings.Count(rr.Body.String(), "event: response.incomplete") != 1 {
		t.Fatal("output limit or repeated terminal markers were handled incorrectly")
	}
}

func TestQwenResponsesStreamHandlesEverySSEFragmentBoundary(t *testing.T) {
	prepared := qwenPreparedTestRequest(t, map[string]any{"input": "Say hello", "stream": true})
	stream := strings.ReplaceAll(qwenChatSSE(map[string]any{"content": "Olá ☕"}, "stop")+"data: [DONE]\n\n", "\n", "\r\n")
	for split := 0; split <= len(stream); split++ {
		var output bytes.Buffer
		writer := newQwenResponsesStream(&output, prepared, nil)
		if _, err := writer.Write([]byte(stream[:split])); err != nil {
			t.Fatalf("first fragment %d: %v", split, err)
		}
		if _, err := writer.Write([]byte(stream[split:])); err != nil {
			t.Fatalf("second fragment %d: %v", split, err)
		}
		events := qwenResponsesEvents(t, output.Bytes())
		last := events[len(events)-1]
		if last["type"] != "response.completed" || !strings.Contains(output.String(), "Olá ☕") {
			t.Fatalf("split %d lost Unicode text or terminal state", split)
		}
	}
}

type qwenBrokenWriter struct{ calls int }

func (w *qwenBrokenWriter) Write([]byte) (int, error) { w.calls++; return 0, io.ErrClosedPipe }

func TestQwenResponsesStreamStopsOnClientWriteFailure(t *testing.T) {
	prepared := qwenPreparedTestRequest(t, map[string]any{"input": "hello", "stream": true})
	broken := &qwenBrokenWriter{}
	writer := newQwenResponsesStream(broken, prepared, nil)
	_, err := writer.Write([]byte(qwenChatSSE(map[string]any{"content": "hello"}, "stop")))
	if !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal("downstream failure was lost")
	}
	writer.fail(err)
	writer.Write([]byte("data: [DONE]\n\n"))
	if broken.calls != 1 {
		t.Fatal("broken client connection was written again")
	}
}

func TestQwenResponsesRejectsMismatchedWireFormatAndInvalidJSON(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(fmt.Sprint(stream), func(t *testing.T) {
			prepared := qwenPreparedTestRequest(t, map[string]any{"input": "hello", "stream": stream})
			response := qwenJSONResponse(200, `{`)
			if !stream {
				response.Header.Set("Content-Type", "text/event-stream")
			}
			rr := httptest.NewRecorder()
			if writeQwenResponsesResponse(rr, response, prepared, 0, context.CancelFunc(func() {}), nil) == nil || rr.Code != http.StatusBadGateway {
				t.Fatal("unexpected upstream format was passed to Codex")
			}
		})
	}
}
