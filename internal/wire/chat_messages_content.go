package wire

import (
	"encoding/json"
	"fmt"
	"strings"
)

// The two protocols decode into the same portable content used by Conversation.
// Message grouping and required envelope fields are handled by the destination.
func decodeAnthropicContent(value any) ([]Content, error) {
	if text, ok := value.(string); ok {
		return []Content{{Type: ContentText, Text: text}}, nil
	}
	blocks, _ := value.([]any)
	var parts []Content
	for _, raw := range blocks {
		block, _ := raw.(map[string]any)
		switch block["type"] {
		case "text":
			if text, ok := block["text"].(string); ok {
				parts = append(parts, Content{Type: ContentText, Text: text})
			}
		case "image":
			source, _ := block["source"].(map[string]any)
			mediaType, _ := source["media_type"].(string)
			data, _ := source["data"].(string)
			if mediaType != "" && data != "" {
				parts = append(parts, Content{Type: ContentImage, MimeType: mediaType, Data: data})
			}
		case "thinking":
			if text, ok := block["thinking"].(string); ok && text != "" {
				parts = append(parts, Content{Type: ContentReasoningVisible, Text: text})
			}
		case "tool_use":
			id, _ := block["id"].(string)
			name, _ := block["name"].(string)
			arguments, err := toolArguments(block["input"])
			if err != nil {
				return nil, fmt.Errorf("Anthropic tool %q: %w", id, err)
			}
			parts = append(parts, Content{Type: ContentToolCall, ToolID: id, ToolName: name, Arguments: arguments})
		case "tool_result":
			id, _ := block["tool_use_id"].(string)
			parts = append(parts, Content{Type: ContentToolResult, ToolID: id, Text: AnthropicToolResultText(block["content"])})
		}
	}
	return parts, nil
}

// AnthropicToolResultText extracts the printable text from an Anthropic tool
// result for protocols that use a string result.
func AnthropicToolResultText(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	blocks, _ := value.([]any)
	var text []string
	for _, raw := range blocks {
		block, _ := raw.(map[string]any)
		if part, ok := block["text"].(string); ok {
			text = append(text, part)
		}
	}
	return strings.Join(text, "\n\n")
}

func decodeChatContent(value any) []Content {
	if text, ok := value.(string); ok && text != "" {
		return []Content{{Type: ContentText, Text: text}}
	}
	items, _ := value.([]any)
	var parts []Content
	for _, raw := range items {
		item, _ := raw.(map[string]any)
		switch item["type"] {
		case "text":
			if text, ok := item["text"].(string); ok && text != "" {
				parts = append(parts, Content{Type: ContentText, Text: text})
			}
		case "image_url":
			image, _ := item["image_url"].(map[string]any)
			if url, ok := image["url"].(string); ok && url != "" {
				parts = append(parts, Content{Type: ContentImage, ImageURL: url})
			}
		}
	}
	return parts
}

func decodeChatToolCalls(raw any) ([]Content, error) {
	calls, _ := raw.([]any)
	var parts []Content
	for _, rawCall := range calls {
		call, _ := rawCall.(map[string]any)
		function, _ := call["function"].(map[string]any)
		if function == nil {
			continue
		}
		id, _ := call["id"].(string)
		name, _ := function["name"].(string)
		arguments, err := toolArguments(function["arguments"])
		if err != nil {
			return nil, fmt.Errorf("OpenAI Chat tool %q: %w", id, err)
		}
		parts = append(parts, Content{Type: ContentToolCall, ToolID: id, ToolName: name, Arguments: arguments})
	}
	return parts, nil
}

func toolArguments(value any) (string, error) {
	if value == nil {
		return "", fmt.Errorf("missing tool arguments")
	}
	if text, ok := value.(string); ok {
		return text, nil
	}
	data, err := json.Marshal(value)
	return string(data), err
}

func encodeChatToolCall(part Content) map[string]any {
	return map[string]any{
		"id": part.ToolID, "type": "function",
		"function": map[string]any{"name": part.ToolName, "arguments": part.Arguments},
	}
}

func encodeAnthropicBlocks(parts []Content) ([]any, error) {
	var blocks []any
	for _, part := range parts {
		switch part.Type {
		case ContentText:
			blocks = append(blocks, map[string]any{"type": "text", "text": part.Text})
		case ContentImage:
			source := map[string]any{"type": "url", "url": part.ImageURL}
			if data, ok := strings.CutPrefix(part.ImageURL, "data:"); ok {
				mediaType, body, ok := strings.Cut(data, ";base64,")
				if !ok {
					return nil, fmt.Errorf("invalid image data URL")
				}
				source = map[string]any{"type": "base64", "media_type": mediaType, "data": body}
			}
			blocks = append(blocks, map[string]any{"type": "image", "source": source})
		case ContentReasoningVisible:
			blocks = append(blocks, map[string]any{"type": "thinking", "thinking": part.Text})
		case ContentToolCall:
			var input any
			if err := json.Unmarshal([]byte(part.Arguments), &input); err != nil {
				return nil, fmt.Errorf("OpenAI Chat tool %q has invalid arguments: %w", part.ToolID, err)
			}
			blocks = append(blocks, map[string]any{
				"type": "tool_use", "id": part.ToolID, "name": part.ToolName, "input": input,
			})
		}
	}
	return blocks, nil
}
