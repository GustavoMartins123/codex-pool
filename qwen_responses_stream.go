package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/google/uuid"
)

type qwenStreamTool struct {
	ID, Name, Arguments string
	OutputIndex         int
	EmittedArguments    int
}

type qwenResponsesStream struct {
	prepared               *qwenResponsesRequest
	w                      io.Writer
	framer                 sseFramer
	buffer                 []byte
	err                    error
	writeErr               error
	received               int
	id                     string
	createdAt              int64
	sequence               int
	started, terminal      bool
	finishReason           string
	output                 []any
	textIndex, reasonIndex int
	tools                  map[int]*qwenStreamTool
	usage                  map[string]any
	onPayload              func([]byte)
}

func newQwenResponsesStream(w io.Writer, prepared *qwenResponsesRequest, onPayload func([]byte)) *qwenResponsesStream {
	return &qwenResponsesStream{prepared: prepared, w: w, id: "resp_" + uuid.NewString(), createdAt: time.Now().Unix(), textIndex: -1, reasonIndex: -1, tools: map[int]*qwenStreamTool{}, output: []any{}, onPayload: onPayload}
}

func (s *qwenResponsesStream) response(status string) map[string]any {
	response := map[string]any{"id": s.id, "object": "response", "created_at": s.createdAt, "status": status, "background": false, "model": s.prepared.Model, "output": s.output, "error": nil, "incomplete_details": nil}
	if s.usage != nil {
		response["usage"] = s.usage
	}
	if status == "incomplete" {
		reason := "max_output_tokens"
		if s.finishReason == "content_filter" {
			reason = "content_filter"
		}
		response["incomplete_details"] = map[string]any{"reason": reason}
	}
	for _, key := range []string{"instructions", "tools", "tool_choice", "parallel_tool_calls", "max_output_tokens", "reasoning", "text", "metadata", "store", "temperature", "top_p"} {
		if value, exists := s.prepared.Request[key]; exists {
			response[key] = value
		}
	}
	return response
}

func (s *qwenResponsesStream) emit(kind string, data map[string]any) {
	if s.w == nil || s.err != nil {
		return
	}
	data["type"], data["sequence_number"] = kind, s.sequence
	s.sequence++
	encoded, err := json.Marshal(data)
	if err != nil {
		s.err = err
		return
	}
	writeSSE(s.w, []byte("event: "+kind+"\ndata: "+string(encoded)+"\n\n"), &s.err)
	if s.err != nil {
		s.writeErr = s.err
	}
	if flusher, ok := s.w.(http.Flusher); ok && s.err == nil {
		flusher.Flush()
	}
}

func (s *qwenResponsesStream) start() {
	if s.started {
		return
	}
	s.started = true
	s.emit("response.created", map[string]any{"response": s.response("in_progress")})
	s.emit("response.in_progress", map[string]any{"response": s.response("in_progress")})
}

func (s *qwenResponsesStream) addItem(item map[string]any) int {
	index := len(s.output)
	s.output = append(s.output, item)
	s.emit("response.output_item.added", map[string]any{"output_index": index, "item": item})
	return index
}

func (s *qwenResponsesStream) textDelta(value string, reasoning bool) {
	if value == "" {
		return
	}
	if reasoning {
		if s.reasonIndex < 0 {
			s.reasonIndex = s.addItem(map[string]any{"id": "rs_" + uuid.NewString(), "type": "reasoning", "summary": []any{}})
			s.emit("response.reasoning_summary_part.added", map[string]any{"item_id": mapValue(s.output[s.reasonIndex])["id"], "output_index": s.reasonIndex, "summary_index": 0, "part": map[string]any{"type": "summary_text", "text": ""}})
		}
		item := mapValue(s.output[s.reasonIndex])
		text := ""
		if summary := anySlice(item["summary"]); len(summary) > 0 {
			text = stringValue(mapValue(summary[0])["text"])
		}
		item["summary"] = []any{map[string]any{"type": "summary_text", "text": text + value}}
		s.emit("response.reasoning_summary_text.delta", map[string]any{"item_id": item["id"], "output_index": s.reasonIndex, "summary_index": 0, "delta": value})
		return
	}
	if s.textIndex < 0 {
		s.textIndex = s.addItem(map[string]any{"id": "msg_" + uuid.NewString(), "type": "message", "role": "assistant", "status": "in_progress", "content": []any{}})
		s.emit("response.content_part.added", map[string]any{"item_id": mapValue(s.output[s.textIndex])["id"], "output_index": s.textIndex, "content_index": 0, "part": map[string]any{"type": "output_text", "text": "", "annotations": []any{}, "logprobs": []any{}}})
	}
	item := mapValue(s.output[s.textIndex])
	text := ""
	if content := anySlice(item["content"]); len(content) > 0 {
		text = stringValue(mapValue(content[0])["text"])
	}
	item["content"] = []any{map[string]any{"type": "output_text", "text": text + value, "annotations": []any{}, "logprobs": []any{}}}
	s.emit("response.output_text.delta", map[string]any{"item_id": item["id"], "output_index": s.textIndex, "content_index": 0, "delta": value, "logprobs": []any{}})
}

func (s *qwenResponsesStream) toolDelta(raw map[string]any) error {
	number, ok := raw["index"].(float64)
	if !ok || number < 0 || number > 1024 || number != float64(int(number)) {
		return fmt.Errorf("Qwen returned an invalid tool call index")
	}
	index := int(number)
	tool := s.tools[index]
	if tool == nil {
		tool = &qwenStreamTool{OutputIndex: -1}
		s.tools[index] = tool
	}
	if kind := stringValue(raw["type"]); kind != "" && kind != "function" {
		return fmt.Errorf("Qwen returned an unsupported tool call type")
	}
	if id := stringValue(raw["id"]); id != "" {
		if tool.ID != "" && tool.ID != id {
			return fmt.Errorf("Qwen changed a tool call ID during streaming")
		}
		tool.ID = id
	}
	function := mapValue(raw["function"])
	if name := stringValue(function["name"]); name != "" {
		if tool.OutputIndex >= 0 {
			return fmt.Errorf("Qwen changed a tool name after starting its arguments")
		}
		tool.Name += name
	}
	if raw, exists := function["arguments"]; exists {
		value, ok := raw.(string)
		if !ok {
			return fmt.Errorf("Qwen function arguments must be a string")
		}
		tool.Arguments += value
	}
	meta, found := s.prepared.Tools[tool.Name]
	if !found || !s.prepared.declared[tool.Name] || tool.ID == "" {
		return nil
	}
	if tool.OutputIndex < 0 {
		kind, prefix, field := "function_call", "fc_", "arguments"
		if meta.Type == "custom" {
			kind, prefix, field = "custom_tool_call", "ctc_", "input"
		}
		item := map[string]any{"id": prefix + uuid.NewString(), "type": kind, "status": "in_progress", "call_id": tool.ID, "name": meta.Name, field: ""}
		if meta.Namespace != "" {
			item["namespace"] = meta.Namespace
		}
		tool.OutputIndex = s.addItem(item)
	}
	if meta.Type == "function" && len(tool.Arguments) > tool.EmittedArguments {
		item := mapValue(s.output[tool.OutputIndex])
		s.emit("response.function_call_arguments.delta", map[string]any{"item_id": item["id"], "output_index": tool.OutputIndex, "delta": tool.Arguments[tool.EmittedArguments:]})
		tool.EmittedArguments = len(tool.Arguments)
	}
	return s.err
}

func (s *qwenResponsesStream) consume(data []byte, streaming bool) error {
	if s.onPayload != nil {
		s.onPayload(data)
	}
	var chunk map[string]any
	if json.Unmarshal(data, &chunk) != nil {
		return fmt.Errorf("Qwen returned invalid response JSON")
	}
	if chunk["error"] != nil {
		return fmt.Errorf("Qwen reported an upstream error")
	}
	if !s.started {
		if id := stringValue(chunk["id"]); id != "" {
			s.id = "resp_" + id
		}
		if created := toInt64(chunk["created"]); created > 0 {
			s.createdAt = created
		}
		s.start()
	}
	if usage := mapValue(chunk["usage"]); usage != nil {
		input, output := toInt64(usage["prompt_tokens"]), toInt64(usage["completion_tokens"])
		s.usage = map[string]any{"input_tokens": input, "output_tokens": output, "total_tokens": input + output,
			"input_tokens_details":  map[string]any{"cached_tokens": toInt64(mapValue(usage["prompt_tokens_details"])["cached_tokens"])},
			"output_tokens_details": map[string]any{"reasoning_tokens": toInt64(mapValue(usage["completion_tokens_details"])["reasoning_tokens"])}}
	}
	for _, raw := range anySlice(chunk["choices"]) {
		choice := mapValue(raw)
		if index, exists := choice["index"]; exists && toInt64(index) != 0 {
			return fmt.Errorf("Qwen Responses conversion expects one completion choice")
		}
		message := mapValue(choice["message"])
		if streaming {
			message = mapValue(choice["delta"])
		}
		if message["refusal"] != nil && message["refusal"] != "" {
			return fmt.Errorf("Qwen refused the request")
		}
		s.textDelta(stringValue(message["reasoning_content"]), true)
		s.textDelta(stringValue(message["content"]), false)
		for index, raw := range anySlice(message["tool_calls"]) {
			call := cloneAnyMap(mapValue(raw))
			if !streaming {
				call["index"] = float64(index)
			}
			if err := s.toolDelta(call); err != nil {
				return err
			}
		}
		if reason := stringValue(choice["finish_reason"]); reason != "" {
			if s.finishReason != "" {
				return fmt.Errorf("Qwen returned more than one terminal choice")
			}
			s.finishReason = reason
		}
	}
	return s.err
}

func (s *qwenResponsesStream) finish() error {
	if s.err != nil {
		return s.err
	}
	if s.terminal {
		return nil
	}
	if !s.started || s.finishReason == "" {
		return fmt.Errorf("Qwen stream ended without a terminal completion")
	}
	if s.finishReason != "stop" && s.finishReason != "tool_calls" && s.finishReason != "length" && s.finishReason != "content_filter" {
		return fmt.Errorf("Qwen returned an unsupported completion finish reason")
	}
	indices := make([]int, 0, len(s.tools))
	for index := range s.tools {
		indices = append(indices, index)
	}
	sort.Ints(indices)
	seen := map[string]bool{}
	for _, index := range indices {
		tool := s.tools[index]
		meta, found := s.prepared.Tools[tool.Name]
		if !found || !s.prepared.declared[tool.Name] || tool.OutputIndex < 0 || tool.ID == "" || seen[tool.ID] {
			return fmt.Errorf("Qwen returned an unknown tool, missing ID or duplicate tool call")
		}
		seen[tool.ID] = true
		var args map[string]any
		if json.Unmarshal([]byte(tool.Arguments), &args) != nil || args == nil {
			return fmt.Errorf("Qwen returned incomplete or invalid function arguments")
		}
		item := mapValue(s.output[tool.OutputIndex])
		if meta.Type == "custom" {
			input, ok := args["input"].(string)
			if !ok || len(args) != 1 {
				return fmt.Errorf("Qwen custom tools require exactly one raw input string")
			}
			item["input"] = input
			s.emit("response.custom_tool_call_input.delta", map[string]any{"item_id": item["id"], "output_index": tool.OutputIndex, "delta": input})
			s.emit("response.custom_tool_call_input.done", map[string]any{"item_id": item["id"], "output_index": tool.OutputIndex, "input": input})
		} else {
			item["arguments"] = tool.Arguments
			s.emit("response.function_call_arguments.done", map[string]any{"item_id": item["id"], "output_index": tool.OutputIndex, "arguments": tool.Arguments})
		}
	}
	if s.finishReason == "tool_calls" && len(s.tools) == 0 {
		return fmt.Errorf("Qwen finished with tool_calls but returned no calls")
	}
	if parallel, ok := s.prepared.Request["parallel_tool_calls"].(bool); ok && !parallel && len(s.tools) > 1 {
		return fmt.Errorf("Qwen returned parallel calls when parallel_tool_calls is false")
	}
	for index, raw := range s.output {
		item := mapValue(raw)
		if item["type"] == "message" {
			part := mapValue(anySlice(item["content"])[0])
			s.emit("response.output_text.done", map[string]any{"item_id": item["id"], "output_index": index, "content_index": 0, "text": part["text"], "logprobs": []any{}})
			s.emit("response.content_part.done", map[string]any{"item_id": item["id"], "output_index": index, "content_index": 0, "part": part})
		} else if item["type"] == "reasoning" {
			part := mapValue(anySlice(item["summary"])[0])
			s.emit("response.reasoning_summary_text.done", map[string]any{"item_id": item["id"], "output_index": index, "summary_index": 0, "text": part["text"]})
			s.emit("response.reasoning_summary_part.done", map[string]any{"item_id": item["id"], "output_index": index, "summary_index": 0, "part": part})
		}
		item["status"] = "completed"
		s.emit("response.output_item.done", map[string]any{"output_index": index, "item": item})
	}
	status := "completed"
	if s.finishReason == "length" || s.finishReason == "content_filter" {
		status = "incomplete"
	}
	s.emit("response."+status, map[string]any{"response": s.response(status)})
	s.terminal = true
	return s.err
}

func (s *qwenResponsesStream) Write(data []byte) (int, error) {
	if s.err != nil {
		return 0, s.err
	}
	s.received += len(data)
	if s.received > 32<<20 {
		s.err = fmt.Errorf("Qwen stream exceeds the response size limit")
		return 0, s.err
	}
	s.buffer = append(s.buffer, data...)
	if len(s.buffer) > 8<<20 {
		s.err = fmt.Errorf("Qwen SSE event exceeds the size limit")
		return 0, s.err
	}
	for {
		event, consumed, ok := s.framer.next(s.buffer)
		if !ok {
			break
		}
		s.buffer = s.buffer[consumed:]
		_, payload := parseSSEEvent(event)
		if len(payload) == 0 || s.terminal {
			continue
		}
		if bytes.Equal(payload, []byte("[DONE]")) {
			s.err = s.finish()
		} else {
			s.err = s.consume(payload, true)
		}
		if s.err != nil {
			return len(data), s.err
		}
	}
	return len(data), nil
}

func (s *qwenResponsesStream) fail(err error) {
	if s.terminal || s.writeErr != nil {
		return
	}
	s.err = nil
	s.start()
	response := s.response("failed")
	response["error"] = map[string]any{"code": "upstream_protocol_error", "message": err.Error()}
	s.emit("response.failed", map[string]any{"response": response})
	s.terminal = true
}

func writeQwenResponsesResponse(w http.ResponseWriter, response *http.Response, prepared *qwenResponsesRequest, idleTimeout time.Duration, cancel context.CancelFunc, onPayload func([]byte)) error {
	defer response.Body.Close()
	w.Header().Del("Content-Length")
	w.Header().Del("Content-Encoding")
	isSSE := strings.Contains(strings.ToLower(response.Header.Get("Content-Type")), "text/event-stream")
	if prepared.Stream != isSSE {
		err := fmt.Errorf("Qwen returned a response format that does not match stream=%t", prepared.Stream)
		respondJSONError(w, http.StatusBadGateway, err.Error())
		return err
	}
	if !prepared.Stream {
		data, err := io.ReadAll(io.LimitReader(response.Body, (32<<20)+1))
		if err != nil || len(data) > 32<<20 {
			err = fmt.Errorf("could not read a complete Qwen response within the size limit")
			respondJSONError(w, http.StatusBadGateway, err.Error())
			return err
		}
		state := newQwenResponsesStream(nil, prepared, onPayload)
		err = state.consume(data, false)
		if err == nil {
			err = state.finish()
		}
		if err != nil {
			respondJSONError(w, http.StatusBadGateway, err.Error())
			return err
		}
		status := "completed"
		if state.finishReason == "length" || state.finishReason == "content_filter" {
			status = "incomplete"
		}
		respondJSON(w, state.response(status))
		return nil
	}
	applyStreamingResponseHeaders(w.Header())
	w.Header().Set("Content-Type", "text/event-stream")
	w.WriteHeader(http.StatusOK)
	state := newQwenResponsesStream(w, prepared, onPayload)
	var reader io.Reader = response.Body
	if idleTimeout > 0 {
		idle := newIdleTimeoutReader(response.Body, idleTimeout, cancel)
		defer idle.Close()
		reader = idle
	}
	_, err := io.Copy(state, reader)
	if err == nil && !state.terminal {
		err = fmt.Errorf("Qwen stream ended before its [DONE] marker")
	}
	if err != nil {
		state.fail(err)
	}
	return err
}
