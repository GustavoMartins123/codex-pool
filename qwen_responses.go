package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"regexp"
	"strings"

	"codex-pool-proxy/internal/jsonvalue"
)

type qwenResponseTool struct {
	Name, Namespace, Type, UpstreamName string
}

type qwenResponsesRequest struct {
	Body      []byte
	Request   map[string]any
	Model     string
	Stream    bool
	Tools     map[string]qwenResponseTool
	toolNames map[string]string
	declared  map[string]bool
}

var (
	qwenFunctionNamePattern = regexp.MustCompile(`^[A-Za-z0-9_-]{1,64}$`)
	qwenFunctionUnsafeName  = regexp.MustCompile(`[^A-Za-z0-9_-]`)
)

func qwenResponseToolKey(namespace, name string) string { return namespace + "\x00" + name }

func (p *qwenResponsesRequest) registerTool(namespace, name, kind string) (qwenResponseTool, error) {
	if name == "" || strings.TrimSpace(name) != name {
		return qwenResponseTool{}, fmt.Errorf("Qwen tools require a nonempty name without surrounding whitespace")
	}
	key := qwenResponseToolKey(namespace, name)
	if upstream, found := p.toolNames[key]; found {
		tool := p.Tools[upstream]
		if tool.Type != kind {
			return qwenResponseTool{}, fmt.Errorf("tool %q has conflicting function and custom declarations", name)
		}
		return tool, nil
	}
	upstream := name
	if namespace != "" || !qwenFunctionNamePattern.MatchString(name) {
		digest := sha256.Sum256([]byte(key))
		prefix := qwenFunctionUnsafeName.ReplaceAllString(namespace+"_"+name, "_")
		if len(prefix) > 43 {
			prefix = prefix[:43]
		}
		upstream = prefix + "_" + hex.EncodeToString(digest[:10])
	}
	if previous, exists := p.Tools[upstream]; exists && (previous.Namespace != namespace || previous.Name != name) {
		return qwenResponseTool{}, fmt.Errorf("Qwen tool names collide after encoding")
	}
	tool := qwenResponseTool{Name: name, Namespace: namespace, Type: kind, UpstreamName: upstream}
	p.Tools[upstream], p.toolNames[key] = tool, upstream
	return tool, nil
}

func (p *qwenResponsesRequest) convertTools(raw any, namespace string) ([]any, error) {
	tools, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("Qwen tool declarations must be an array")
	}
	var out []any
	for index, raw := range tools {
		tool, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("tools[%d] must be an object", index)
		}
		kind := jsonvalue.String(tool["type"])
		name := jsonvalue.String(tool["name"])
		if kind == "namespace" {
			if name == "" {
				return nil, fmt.Errorf("tool namespace requires a name")
			}
			if namespace != "" {
				name = namespace + "." + name
			}
			children, err := p.convertTools(tool["tools"], name)
			if err != nil {
				return nil, err
			}
			out = append(out, children...)
			continue
		}
		if kind != "function" && kind != "custom" {
			return nil, fmt.Errorf("Qwen Responses conversion does not support tool type %q", kind)
		}
		meta, err := p.registerTool(namespace, name, kind)
		if err != nil {
			return nil, err
		}
		if p.declared[meta.UpstreamName] {
			return nil, fmt.Errorf("duplicate Qwen tool declaration %q", name)
		}
		p.declared[meta.UpstreamName] = true
		description := jsonvalue.String(tool["description"])
		params, _ := tool["parameters"].(map[string]any)
		if kind == "custom" {
			description += "\nProvide the raw tool input in the input string."
			if format := jsonvalue.Map(tool["format"]); format != nil {
				switch jsonvalue.String(format["type"]) {
				case "text":
				case "grammar":
					definition := jsonvalue.String(format["definition"])
					if definition == "" {
						return nil, fmt.Errorf("custom tool %q requires a grammar definition", name)
					}
					description += "\nInput grammar (" + jsonvalue.String(format["syntax"]) + "):\n" + definition
				default:
					return nil, fmt.Errorf("custom tool %q has an unsupported input format", name)
				}
			}
			params = map[string]any{"type": "object", "properties": map[string]any{"input": map[string]any{"type": "string"}}, "required": []any{"input"}, "additionalProperties": false}
		}
		if params == nil || params["type"] != "object" {
			return nil, fmt.Errorf("tool %q requires an object parameters schema", name)
		}
		function := map[string]any{"name": meta.UpstreamName, "parameters": params}
		if description != "" {
			function["description"] = description
		}
		out = append(out, map[string]any{"type": "function", "function": function})
	}
	return out, nil
}

func prepareQwenResponsesRequest(path string, body []byte, model string) (*qwenResponsesRequest, error) {
	if strings.HasSuffix(normalizeNoopPath(path), "/compact") {
		return nil, fmt.Errorf("Qwen does not support the Responses compaction endpoint")
	}
	var root map[string]any
	if json.Unmarshal(body, &root) != nil || root == nil {
		return nil, fmt.Errorf("Qwen Responses request must be a JSON object")
	}
	p := &qwenResponsesRequest{Request: root, Model: model, Tools: map[string]qwenResponseTool{}, toolNames: map[string]string{}, declared: map[string]bool{}}
	if raw, exists := root["stream"]; exists {
		var ok bool
		p.Stream, ok = raw.(bool)
		if !ok {
			return nil, fmt.Errorf("stream must be a boolean")
		}
	}
	out := map[string]any{"model": model, "stream": p.Stream}
	var tools []any
	if raw, exists := root["tools"]; exists {
		converted, err := p.convertTools(raw, "")
		if err != nil {
			return nil, err
		}
		tools = append(tools, converted...)
	}
	for _, raw := range jsonvalue.Slice(root["input"]) {
		item := jsonvalue.Map(raw)
		if item["type"] == "additional_tools" {
			converted, err := p.convertTools(item["tools"], "")
			if err != nil {
				return nil, err
			}
			tools = append(tools, converted...)
		}
	}
	if len(tools) > 0 {
		out["tools"] = tools
	}
	messages, err := p.convertInput(root["input"])
	if err != nil {
		return nil, err
	}
	if instructions, exists := root["instructions"]; exists && instructions != nil {
		text, ok := instructions.(string)
		if !ok {
			return nil, fmt.Errorf("instructions must be a string")
		}
		messages = append([]any{map[string]any{"role": "system", "content": text}}, messages...)
	}
	if len(messages) == 0 {
		return nil, fmt.Errorf("Qwen requires at least one input message")
	}
	out["messages"] = messages
	for _, key := range []string{"temperature", "top_p", "parallel_tool_calls"} {
		if value, exists := root[key]; exists {
			out[key] = value
		}
	}
	if value, exists := root["parallel_tool_calls"]; exists {
		if _, ok := value.(bool); !ok {
			return nil, fmt.Errorf("parallel_tool_calls must be a boolean")
		}
	}
	if value, exists := root["max_output_tokens"]; exists {
		out["max_tokens"] = value
	}
	if reasoning := jsonvalue.Map(root["reasoning"]); reasoning != nil {
		applyQwenReasoningEffort(model, jsonvalue.String(reasoning["effort"]), out)
	}
	if format := jsonvalue.Map(jsonvalue.Map(root["text"])["format"]); format != nil {
		switch format["type"] {
		case "text":
		case "json_object":
			out["response_format"] = map[string]any{"type": "json_object"}
		case "json_schema":
			schema := cloneAnyMap(format)
			delete(schema, "type")
			out["response_format"] = map[string]any{"type": "json_schema", "json_schema": schema}
		default:
			return nil, fmt.Errorf("unsupported Qwen text format")
		}
	}
	if err := p.convertToolChoice(root["tool_choice"], out); err != nil {
		return nil, err
	}
	encoded, err := json.Marshal(out)
	if err != nil {
		return nil, err
	}
	p.Body, err = prepareQwenChatRequest(encoded)
	return p, err
}

func (p *qwenResponsesRequest) convertToolChoice(raw any, out map[string]any) error {
	if raw == nil {
		return nil
	}
	if choice, ok := raw.(string); ok {
		if choice != "auto" && choice != "none" {
			return fmt.Errorf("Qwen tool_choice supports auto, none or a specific function; required is unsupported")
		}
		out["tool_choice"] = choice
		return nil
	}
	choice, ok := raw.(map[string]any)
	if !ok || (choice["type"] != "function" && choice["type"] != "custom") {
		return fmt.Errorf("unsupported Qwen tool_choice")
	}
	name := p.toolNames[qwenResponseToolKey(jsonvalue.String(choice["namespace"]), jsonvalue.String(choice["name"]))]
	if !p.declared[name] {
		return fmt.Errorf("Qwen tool_choice must refer to a declared tool")
	}
	out["tool_choice"] = map[string]any{"type": "function", "function": map[string]any{"name": name}}
	return nil
}

func (p *qwenResponsesRequest) convertInput(raw any) ([]any, error) {
	if text, ok := raw.(string); ok {
		return []any{map[string]any{"role": "user", "content": text}}, nil
	}
	items, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("Responses input must be a string or array; chained requests must include their complete history")
	}
	var messages []any
	var pending map[string]any
	var reasoning strings.Builder
	flush := func() {
		if pending != nil {
			messages = append(messages, pending)
			pending = nil
		}
	}
	results := map[string]bool{}
	calls := map[string]bool{}
	for index, raw := range items {
		item, ok := raw.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("input[%d] must be an object", index)
		}
		kind := jsonvalue.String(item["type"])
		switch kind {
		case "additional_tools":
			continue
		case "reasoning":
			// Only visible summaries carry reasoning Qwen can read. Encrypted
			// reasoning belongs to the provider that produced it (for example a
			// GPT turn earlier in a conversation that switched to Qwen), so it
			// is left out while the rest of the history is kept.
			for _, part := range jsonvalue.Slice(item["summary"]) {
				reasoning.WriteString(jsonvalue.String(jsonvalue.Map(part)["text"]))
			}
		case "function_call", "custom_tool_call":
			if pending == nil && len(calls) != len(results) {
				return nil, fmt.Errorf("all parallel tool results must precede the next assistant turn")
			}
			id := jsonvalue.String(item["call_id"])
			if id == "" || calls[id] {
				return nil, fmt.Errorf("input[%d] requires a unique function call_id", index)
			}
			toolType := "function"
			if kind == "custom_tool_call" {
				toolType = "custom"
			}
			tool, err := p.registerTool(jsonvalue.String(item["namespace"]), jsonvalue.String(item["name"]), toolType)
			if err != nil {
				return nil, err
			}
			args, ok := item["arguments"].(string)
			if toolType == "custom" {
				input, valid := item["input"].(string)
				if !valid {
					return nil, fmt.Errorf("custom tool input must be a string")
				}
				encoded, _ := json.Marshal(map[string]string{"input": input})
				args, ok = string(encoded), true
			}
			var object map[string]any
			if !ok || json.Unmarshal([]byte(args), &object) != nil || object == nil {
				return nil, fmt.Errorf("function arguments must contain a JSON object")
			}
			if pending == nil {
				pending = map[string]any{"role": "assistant", "content": nil, "tool_calls": []any{}}
			}
			if reasoning.Len() > 0 {
				pending["reasoning_content"] = reasoning.String()
				reasoning.Reset()
			}
			pending["tool_calls"] = append(jsonvalue.Slice(pending["tool_calls"]), map[string]any{"id": id, "type": "function", "function": map[string]any{"name": tool.UpstreamName, "arguments": args}})
			calls[id] = true
		case "function_call_output", "custom_tool_call_output":
			flush()
			id := jsonvalue.String(item["call_id"])
			if !calls[id] || results[id] {
				return nil, fmt.Errorf("tool output requires a unique preceding call; include the complete Responses history")
			}
			content, err := qwenToolOutput(item["output"])
			if err != nil {
				return nil, err
			}
			messages = append(messages, map[string]any{"role": "tool", "tool_call_id": id, "content": content})
			results[id] = true
		case "message", "":
			if len(calls) != len(results) {
				return nil, fmt.Errorf("all tool results must precede the next message")
			}
			flush()
			role := jsonvalue.String(item["role"])
			if role == "developer" {
				role = "system"
			}
			if role != "system" && role != "user" && role != "assistant" {
				return nil, fmt.Errorf("input[%d] has unsupported message role %q", index, role)
			}
			content, err := qwenMessageContent(item["content"])
			if err != nil {
				return nil, err
			}
			message := map[string]any{"role": role, "content": content}
			if role == "assistant" && reasoning.Len() > 0 {
				message["reasoning_content"] = reasoning.String()
				reasoning.Reset()
			}
			messages = append(messages, message)
		default:
			return nil, fmt.Errorf("input[%d] has unsupported Responses item type %q", index, kind)
		}
	}
	flush()
	for id := range calls {
		if !results[id] {
			return nil, fmt.Errorf("function call %q is missing its output in Responses history", id)
		}
	}
	return messages, nil
}

func qwenToolOutput(raw any) (string, error) {
	if text, ok := raw.(string); ok {
		return text, nil
	}
	if parts, ok := raw.([]any); ok {
		var text strings.Builder
		for _, raw := range parts {
			part := jsonvalue.Map(raw)
			if part["type"] != "input_text" && part["type"] != "output_text" && part["type"] != "text" {
				return "", fmt.Errorf("Qwen tool results currently require text content")
			}
			value, ok := part["text"].(string)
			if !ok {
				return "", fmt.Errorf("tool result text must be a string")
			}
			text.WriteString(value)
		}
		return text.String(), nil
	}
	return "", fmt.Errorf("Qwen tool results must be strings or text content arrays")
}

func qwenMessageContent(raw any) (any, error) {
	if text, ok := raw.(string); ok {
		return text, nil
	}
	parts, ok := raw.([]any)
	if !ok {
		return nil, fmt.Errorf("Qwen message content must be a string or array")
	}
	out := make([]any, 0, len(parts))
	for _, raw := range parts {
		part := jsonvalue.Map(raw)
		switch part["type"] {
		case "input_text", "output_text", "text":
			text, ok := part["text"].(string)
			if !ok {
				return nil, fmt.Errorf("message text must be a string")
			}
			out = append(out, map[string]any{"type": "text", "text": text})
		case "input_image":
			image := jsonvalue.String(part["image_url"])
			if image == "" {
				return nil, fmt.Errorf("Qwen input images require an image_url")
			}
			value := map[string]any{"url": image}
			if detail, exists := part["detail"]; exists {
				value["detail"] = detail
			}
			out = append(out, map[string]any{"type": "image_url", "image_url": value})
		default:
			return nil, fmt.Errorf("unsupported Qwen message content type %q", part["type"])
		}
	}
	return out, nil
}
