package wire

import (
	"encoding/json"
	"fmt"
	"strings"

	"codex-pool-proxy/internal/orderedjson"
)

var anthropicRequestKeyOrder = []string{
	"model", "messages", "system", "tools", "metadata",
	"max_tokens", "thinking", "context_management",
	"output_config", "stream",
}

// MarshalAnthropicRequest orders the envelope like a native Messages request.
func MarshalAnthropicRequest(body map[string]any) ([]byte, error) {
	return orderedjson.Marshal(body, anthropicRequestKeyOrder)
}

func decodeJSONObject(body []byte, description string) (map[string]any, error) {
	var object map[string]any
	if err := json.Unmarshal(body, &object); err != nil {
		return nil, fmt.Errorf("parse %s: %w", description, err)
	}
	if object == nil {
		return nil, fmt.Errorf("parse %s: expected a JSON object", description)
	}
	return object, nil
}

// AnthropicRequestToOpenAIChat translates a Messages request to Chat Completions.
func AnthropicRequestToOpenAIChat(body []byte) ([]byte, error) {
	source, err := decodeJSONObject(body, "claude request")
	if err != nil {
		return nil, err
	}
	chat := make(map[string]any)
	var messages []map[string]any
	if system := AnthropicToolResultText(source["system"]); system != "" {
		messages = append(messages, map[string]any{"role": "system", "content": system})
	}
	rawMessages, _ := source["messages"].([]any)
	for _, raw := range rawMessages {
		message, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := message["role"].(string)
		parts, err := decodeAnthropicContent(message["content"])
		if err != nil {
			return nil, err
		}
		messages = append(messages, encodeChatHistory(Message{Role: Role(role), Content: parts})...)
	}
	chat["messages"] = messages
	copyCodecFields(chat, source, "model", "temperature", "top_p", "max_tokens")
	if stop, ok := source["stop_sequences"]; ok {
		chat["stop"] = stop
	}
	if stream, ok := source["stream"].(bool); ok {
		chat["stream"] = stream
		if stream {
			chat["stream_options"] = map[string]any{"include_usage": true}
		}
	}
	if tools, ok := source["tools"].([]any); ok && len(tools) > 0 {
		chat["tools"] = anthropicToolsToChat(tools)
	}
	if choice, ok := source["tool_choice"]; ok {
		chat["tool_choice"], err = anthropicChoiceToChat(choice)
		if err != nil {
			return nil, err
		}
	}
	return json.Marshal(chat)
}

func encodeChatHistory(message Message) []map[string]any {
	var text []string
	var parts, calls, results []map[string]any
	hasImage := false
	for _, part := range message.Content {
		switch part.Type {
		case ContentText:
			text = append(text, part.Text)
			parts = append(parts, map[string]any{"type": "text", "text": part.Text})
		case ContentImage:
			url := "data:" + part.MimeType + ";base64," + part.Data
			parts = append(parts, map[string]any{"type": "image_url", "image_url": map[string]any{"url": url}})
			hasImage = true
		case ContentToolCall:
			calls = append(calls, encodeChatToolCall(part))
		case ContentToolResult:
			results = append(results, map[string]any{"role": "tool", "tool_call_id": part.ToolID, "content": part.Text})
		}
	}
	if message.Role == RoleAssistant && len(calls) > 0 {
		var content any
		if len(text) > 0 {
			content = strings.Join(text, "\n\n")
		}
		return []map[string]any{{"role": message.Role, "content": content, "tool_calls": calls}}
	}
	if len(results) > 0 && !hasImage && len(text) == 0 {
		return results
	}
	var content any = strings.Join(text, "\n\n")
	if hasImage {
		content = parts
	}
	// Chat requires tool replies before the accompanying user message.
	return append(results, map[string]any{"role": message.Role, "content": content})
}

// OpenAIChatRequestToAnthropic translates a Chat Completions request. The
// caller supplies its model-name policy so account routing stays outside wire.
func OpenAIChatRequestToAnthropic(body []byte, resolveModel func(string) string) ([]byte, error) {
	source, err := decodeJSONObject(body, "openai request")
	if err != nil {
		return nil, err
	}
	anthropic := make(map[string]any)
	if model, ok := source["model"].(string); ok {
		if resolveModel == nil {
			return nil, fmt.Errorf("missing Anthropic model resolver")
		}
		anthropic["model"] = resolveModel(model)
	}
	var system []string
	var messages []map[string]any
	rawMessages, _ := source["messages"].([]any)
	for _, raw := range rawMessages {
		message, _ := raw.(map[string]any)
		content := message["content"]
		switch message["role"] {
		case "system":
			if text, ok := content.(string); ok {
				system = append(system, text)
			}
		case "tool":
			result := map[string]any{
				"type": "tool_result", "tool_use_id": message["tool_call_id"], "content": content,
			}
			if len(messages) > 0 {
				last := messages[len(messages)-1]
				if blocks, ok := last["content"].([]any); ok && last["role"] == "user" {
					last["content"] = append(blocks, result)
					continue
				}
			}
			messages = append(messages, map[string]any{"role": "user", "content": []any{result}})
		case "user", "assistant":
			converted, err := encodeAnthropicHistory(message)
			if err != nil {
				return nil, err
			}
			messages = append(messages, converted)
		}
	}
	if len(system) > 0 {
		anthropic["system"] = strings.Join(system, "\n\n")
	}
	anthropic["messages"] = messages
	// Messages requires an output limit even when the Chat caller omits one.
	if limit, ok := source["max_tokens"]; ok {
		anthropic["max_tokens"] = limit
	} else if limit, ok := source["max_completion_tokens"]; ok {
		anthropic["max_tokens"] = limit
	} else {
		anthropic["max_tokens"] = 8192
	}
	copyCodecFields(anthropic, source, "temperature", "top_p")
	if stop, ok := source["stop"]; ok {
		if text, ok := stop.(string); ok {
			stop = []string{text}
		}
		anthropic["stop_sequences"] = stop
	}
	if stream, ok := source["stream"].(bool); ok {
		anthropic["stream"] = stream
	}
	if tools, ok := source["tools"].([]any); ok && len(tools) > 0 {
		anthropic["tools"] = chatToolsToAnthropic(tools)
	}
	if choice, ok := source["tool_choice"]; ok {
		anthropic["tool_choice"], err = chatChoiceToAnthropic(choice)
		if err != nil {
			return nil, err
		}
	}
	return MarshalAnthropicRequest(anthropic)
}

func encodeAnthropicHistory(message map[string]any) (map[string]any, error) {
	role, _ := message["role"].(string)
	content := message["content"]
	if _, ok := content.(string); ok && role == "user" {
		return map[string]any{"role": role, "content": content}, nil
	}
	var parts []Content
	if role == "assistant" {
		if reasoning, ok := message["reasoning_content"].(string); ok && reasoning != "" {
			parts = append(parts, Content{Type: ContentReasoningVisible, Text: reasoning})
		}
	}
	parts = append(parts, decodeChatContent(content)...)
	if role == "assistant" {
		calls, err := decodeChatToolCalls(message["tool_calls"])
		if err != nil {
			return nil, err
		}
		parts = append(parts, calls...)
	}
	blocks, err := encodeAnthropicBlocks(parts)
	if err != nil {
		return nil, err
	}
	if len(blocks) > 0 {
		content = blocks
	} else {
		// Keep the empty string, array or null shapes that carry no content.
		switch value := content.(type) {
		case string, nil:
		case []any:
			if len(value) != 0 {
				return nil, fmt.Errorf("unsupported OpenAI Chat %s content", role)
			}
		default:
			return nil, fmt.Errorf("invalid OpenAI Chat %s content", role)
		}
	}
	return map[string]any{"role": role, "content": content}, nil
}

func copyCodecFields(dst, src map[string]any, keys ...string) {
	for _, key := range keys {
		if value, ok := src[key]; ok {
			dst[key] = value
		}
	}
}

func anthropicToolsToChat(tools []any) []map[string]any {
	var converted []map[string]any
	for _, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		params := tool["input_schema"]
		if schema, ok := params.(map[string]any); ok {
			sanitizeChatToolSchema(schema)
		}
		function := map[string]any{"name": tool["name"], "parameters": params}
		if description, ok := tool["description"].(string); ok {
			function["description"] = description
		}
		converted = append(converted, map[string]any{"type": "function", "function": function})
	}
	return converted
}

func sanitizeChatToolSchema(schema map[string]any) {
	if required, ok := schema["required"]; ok && required == nil {
		schema["required"] = []any{}
	}
	switch schema["format"] {
	case "uri", "uri-reference", "iri", "iri-reference", "uri-template",
		"json-pointer", "relative-json-pointer", "regex", "idn-email", "idn-hostname":
		delete(schema, "format")
	}
	if props, ok := schema["properties"].(map[string]any); ok {
		for _, value := range props {
			if child, ok := value.(map[string]any); ok {
				sanitizeChatToolSchema(child)
			}
		}
	}
	for _, key := range []string{"items", "additionalProperties"} {
		if child, ok := schema[key].(map[string]any); ok {
			sanitizeChatToolSchema(child)
		}
	}
	for _, key := range []string{"allOf", "anyOf", "oneOf"} {
		if children, ok := schema[key].([]any); ok {
			for _, child := range children {
				if child, ok := child.(map[string]any); ok {
					sanitizeChatToolSchema(child)
				}
			}
		}
	}
}

func chatToolsToAnthropic(tools []any) []map[string]any {
	var converted []map[string]any
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		function, _ := tool["function"].(map[string]any)
		if function == nil {
			continue
		}
		entry := map[string]any{"name": function["name"], "input_schema": function["parameters"]}
		if description, ok := function["description"].(string); ok {
			entry["description"] = description
		}
		converted = append(converted, entry)
	}
	return converted
}

func anthropicChoiceToChat(choice any) (any, error) {
	if choice == nil {
		return nil, nil
	}
	kind, _ := choice.(string)
	if object, ok := choice.(map[string]any); ok {
		kind, _ = object["type"].(string)
		if kind == "tool" {
			if name, ok := object["name"].(string); ok && name != "" {
				return map[string]any{"type": "function", "function": map[string]any{"name": name}}, nil
			}
			return nil, fmt.Errorf("Anthropic tool choice requires a name")
		}
	}
	switch kind {
	case "auto", "none":
		return kind, nil
	case "any":
		return "required", nil
	default:
		return nil, fmt.Errorf("unsupported Anthropic tool choice")
	}
}

func chatChoiceToAnthropic(choice any) (any, error) {
	if choice == nil {
		return nil, nil
	}
	if kind, ok := choice.(string); ok {
		switch kind {
		case "auto":
			return map[string]any{"type": "auto"}, nil
		case "required":
			return map[string]any{"type": "any"}, nil
		case "none":
			return map[string]any{"type": "auto", "disable_parallel_tool_use": true}, nil
		}
	}
	if object, ok := choice.(map[string]any); ok {
		if function, ok := object["function"].(map[string]any); ok {
			if name, ok := function["name"].(string); ok && name != "" {
				return map[string]any{"type": "tool", "name": name}, nil
			}
		}
	}
	return nil, fmt.Errorf("unsupported OpenAI Chat tool choice")
}
