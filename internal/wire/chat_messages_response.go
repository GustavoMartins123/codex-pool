package wire

import (
	"encoding/json"
	"fmt"
	"strings"
)

// OpenAIChatResponseToAnthropic translates a completed Chat Completions reply.
func OpenAIChatResponseToAnthropic(body []byte, requestModel string) ([]byte, error) {
	source, err := decodeJSONObject(body, "openai response")
	if err != nil {
		return nil, err
	}
	choices, _ := source["choices"].([]any)
	if len(choices) == 0 {
		return nil, fmt.Errorf("OpenAI Chat response has no choices")
	}
	choice, _ := choices[0].(map[string]any)
	message, _ := choice["message"].(map[string]any)
	if message == nil {
		return nil, fmt.Errorf("OpenAI Chat choice has no message")
	}
	id, _ := source["id"].(string)
	model, _ := source["model"].(string)
	if model == "" {
		model = requestModel
	}

	var parts []Content
	if reasoning, ok := message["reasoning_content"].(string); ok && reasoning != "" {
		parts = append(parts, Content{Type: ContentReasoningVisible, Text: reasoning})
	}
	if details, ok := message["reasoning_details"].([]any); ok {
		for _, raw := range details {
			detail, _ := raw.(map[string]any)
			if text := chatReasoningDetail(detail); text != "" {
				parts = append(parts, Content{Type: ContentReasoningVisible, Text: text})
			}
		}
	}
	if text, ok := message["content"].(string); ok && text != "" {
		parts = append(parts, Content{Type: ContentText, Text: text})
	}
	calls, err := decodeChatToolCalls(message["tool_calls"])
	if err != nil {
		return nil, err
	}
	parts = append(parts, calls...)
	blocks, err := encodeAnthropicBlocks(parts)
	if err != nil {
		return nil, err
	}
	if len(blocks) == 0 {
		blocks = []any{map[string]any{"type": "text", "text": ""}}
	}
	finish, _ := choice["finish_reason"].(string)
	stop := ChatFinishReasonToAnthropic(finish)
	if _, hasTools := message["tool_calls"].([]any); hasTools && stop == "end_turn" {
		stop = "tool_use"
	}
	reply := map[string]any{
		"id": id, "model": model, "type": "message", "role": "assistant",
		"content": blocks, "stop_reason": stop,
	}
	if usage, ok := source["usage"].(map[string]any); ok {
		details, _ := usage["prompt_tokens_details"].(map[string]any)
		input, cached := claudeInputUsage(tokenCount(usage["prompt_tokens"]), tokenCount(details["cached_tokens"]))
		reply["usage"] = map[string]any{
			"input_tokens": input, "cache_read_input_tokens": cached,
			"output_tokens": tokenCount(usage["completion_tokens"]),
		}
	}
	return json.Marshal(reply)
}

// AnthropicResponseToOpenAIChat translates a completed Messages reply.
func AnthropicResponseToOpenAIChat(body []byte) ([]byte, error) {
	source, err := decodeJSONObject(body, "claude response")
	if err != nil {
		return nil, err
	}
	content, ok := source["content"].([]any)
	if !ok {
		return nil, fmt.Errorf("Anthropic response has no content array")
	}
	parts, err := decodeAnthropicContent(content)
	if err != nil {
		return nil, err
	}
	var text, thinking []string
	var calls []map[string]any
	for _, part := range parts {
		switch part.Type {
		case ContentText:
			text = append(text, part.Text)
		case ContentReasoningVisible:
			thinking = append(thinking, part.Text)
		case ContentToolCall:
			calls = append(calls, encodeChatToolCall(part))
		}
	}
	message := map[string]any{"role": "assistant", "content": nil}
	if len(text) > 0 {
		message["content"] = strings.Join(text, "\n\n")
	}
	if len(thinking) > 0 {
		message["reasoning_content"] = strings.Join(thinking, "\n\n")
	}
	if len(calls) > 0 {
		message["tool_calls"] = calls
	}
	id, _ := source["id"].(string)
	model, _ := source["model"].(string)
	stop, _ := source["stop_reason"].(string)
	reply := map[string]any{
		"id": id, "model": model, "object": "chat.completion",
		"choices": []any{map[string]any{
			"index": 0, "message": message, "finish_reason": anthropicStopReasonToChat(stop),
		}},
	}
	if usage, ok := source["usage"].(map[string]any); ok {
		input, output := tokenCount(usage["input_tokens"]), tokenCount(usage["output_tokens"])
		reply["usage"] = map[string]any{"prompt_tokens": input, "completion_tokens": output, "total_tokens": input + output}
	}
	return json.Marshal(reply)
}

// ChatFinishReasonToAnthropic maps completion termination to a Messages stop reason.
func ChatFinishReasonToAnthropic(reason string) string {
	switch reason {
	case "length":
		return "max_tokens"
	case "tool_calls":
		return "tool_use"
	default:
		return "end_turn"
	}
}

func anthropicStopReasonToChat(reason string) string {
	switch reason {
	case "max_tokens":
		return "length"
	case "tool_use":
		return "tool_calls"
	default:
		return "stop"
	}
}

func chatReasoningDetail(detail map[string]any) string {
	switch detail["type"] {
	case "reasoning.summary", "summary":
		text, _ := detail["summary"].(string)
		return text
	case "reasoning.encrypted":
		return ""
	default:
		text, _ := detail["text"].(string)
		return text
	}
}

func tokenCount(value any) int64 {
	// Counts in decoded envelopes are float64, the type used by encoding/json.
	number, _ := value.(float64)
	return int64(number)
}
