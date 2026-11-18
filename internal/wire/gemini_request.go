package wire

// Request conversion is derived in part from CLIProxyAPI's Antigravity
// translators, distributed under the MIT License.

import (
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sort"
	"strings"

	"codex-pool-proxy/internal/jsonvalue"
)

// GeminiSkipThoughtSignature stands in for a thought signature Gemini did not issue.
const GeminiSkipThoughtSignature = "skip_thought_signature_validator"

// ClientRequestToGemini translates a decoded client request into a native
// Gemini generateContent request. For Responses requests it inlines
// additional_tools into root and returns the context that restores the client
// tool names in the reply.
func ClientRequestToGemini(root map[string]any, format RequestFormat, model string) (map[string]any, ResponsesToolContext, error) {
	var tools ResponsesToolContext
	if format == FormatResponses {
		tools.NamespacedTools, tools.CustomTools = antigravityInlineAdditionalTools(root)
	}
	var gemini map[string]any
	var err error
	switch format {
	case FormatGemini:
		gemini = unwrapGeminiClientRequest(root)
		delete(gemini, "model")
	case FormatOpenAI:
		gemini, err = antigravityChatToGemini(root)
	case FormatClaude:
		gemini, err = antigravityAnthropicToGemini(root)
	case FormatResponses:
		gemini, err = antigravityResponsesToGemini(root, model)
	default:
		return nil, ResponsesToolContext{}, fmt.Errorf("unsupported gemini client format %q", format)
	}
	if err != nil {
		return nil, ResponsesToolContext{}, err
	}
	if contents := jsonvalue.Slice(gemini["contents"]); len(contents) > 0 {
		contents = antigravityAlignFunctionResponses(contents)
		gemini["contents"] = antigravityEnsureTrailingUserTurn(contents)
	}
	delete(gemini, "stream")
	delete(gemini, "model")
	delete(gemini, "safetySettings")
	delete(root, "previous_response_id")
	delete(root, "prompt_cache_key")
	if format == FormatResponses {
		tools.Request = cloneMap(root)
		tools.FunctionNames = reverseAntigravityFunctionNameMap(antigravityResponsesFunctionNameMap(root))
	}
	return gemini, tools, nil
}

// unwrapGeminiClientRequest accepts both a raw generateContent body and the
// Gemini CLI / Code Assist envelope {model, request:{contents...}}. Nesting
// the envelope again would send request.request to Google.
func unwrapGeminiClientRequest(root map[string]any) map[string]any {
	request := jsonvalue.Map(root["request"])
	if request != nil {
		if _, ok := request["contents"]; ok {
			return cloneMap(request)
		}
	}
	return cloneMap(root)
}

func cloneMap(input map[string]any) map[string]any {
	result := make(map[string]any, len(input))
	for key, value := range input {
		result[key] = value
	}
	return result
}

func antigravityMergeConsecutiveRoleContents(contents []any) []any {
	if len(contents) <= 1 {
		return contents
	}
	merged := make([]any, 0, len(contents))
	for _, raw := range contents {
		entry := jsonvalue.Map(raw)
		if entry == nil {
			continue
		}
		role := jsonvalue.String(entry["role"])
		parts := jsonvalue.Slice(entry["parts"])
		if len(parts) == 0 {
			continue
		}
		if len(merged) > 0 {
			prev := jsonvalue.Map(merged[len(merged)-1])
			if jsonvalue.String(prev["role"]) == role {
				prevParts := jsonvalue.Slice(prev["parts"])
				prev["parts"] = append(prevParts, parts...)
				continue
			}
		}
		merged = append(merged, map[string]any{
			"role":  role,
			"parts": append([]any(nil), parts...),
		})
	}
	return merged
}

func antigravityAlignFunctionResponses(contents []any) []any {
	type callRef struct {
		id   string
		name string
	}
	var pending []callRef
	for contentIndex, raw := range contents {
		content := jsonvalue.Map(raw)
		parts := jsonvalue.Slice(content["parts"])
		calls := make([]callRef, 0)
		responseParts := make([]int, 0)
		for partIndex, rawPart := range parts {
			part := jsonvalue.Map(rawPart)
			if call := jsonvalue.Map(part["functionCall"]); call != nil {
				calls = append(calls, callRef{id: strings.TrimSpace(jsonvalue.String(call["id"])), name: jsonvalue.String(call["name"])})
			}
			if jsonvalue.Map(part["functionResponse"]) != nil {
				responseParts = append(responseParts, partIndex)
			}
		}
		if len(calls) > 0 {
			pending = calls
			continue
		}
		if len(pending) == 0 || len(responseParts) != len(pending) {
			continue
		}
		byID := make(map[string]map[string]any, len(responseParts))
		for _, partIndex := range responseParts {
			response := jsonvalue.Map(jsonvalue.Map(parts[partIndex])["functionResponse"])
			id := strings.TrimSpace(jsonvalue.String(response["id"]))
			if id == "" {
				byID = nil
				break
			}
			byID[id] = response
		}
		if len(byID) != len(pending) {
			continue
		}
		ordered := make([]any, 0, len(pending))
		complete := true
		for responseIndex, call := range pending {
			response, ok := byID[call.id]
			if !ok || call.id == "" {
				complete = false
				break
			}
			if call.name != "" {
				response["name"] = call.name
			}
			part := cloneMap(jsonvalue.Map(parts[responseParts[responseIndex]]))
			part["functionResponse"] = response
			ordered = append(ordered, part)
		}
		if !complete {
			continue
		}
		for index, partIndex := range responseParts {
			parts[partIndex] = ordered[index]
		}
		content["parts"] = parts
		contents[contentIndex] = content
		pending = nil
	}
	return contents
}

func antigravityChatFunctionNames(messages []any) map[string]string {
	names := make(map[string]string)
	for _, raw := range messages {
		message := jsonvalue.Map(raw)
		for _, rawCall := range jsonvalue.Slice(message["tool_calls"]) {
			call := jsonvalue.Map(rawCall)
			function := jsonvalue.Map(call["function"])
			id := strings.TrimSpace(jsonvalue.String(call["id"]))
			name := sanitizeAntigravityFunctionName(jsonvalue.String(function["name"]))
			if id != "" && name != "" {
				names[id] = name
			}
		}
	}
	return names
}

func antigravityAnthropicFunctionNames(messages []any) map[string]string {
	names := make(map[string]string)
	for _, raw := range messages {
		message := jsonvalue.Map(raw)
		for _, rawBlock := range jsonvalue.Slice(message["content"]) {
			block := jsonvalue.Map(rawBlock)
			if jsonvalue.String(block["type"]) != "tool_use" {
				continue
			}
			id := strings.TrimSpace(jsonvalue.String(block["id"]))
			name := sanitizeAntigravityFunctionName(jsonvalue.String(block["name"]))
			if id != "" && name != "" {
				names[id] = name
			}
		}
	}
	return names
}

func antigravityFunctionResponseName(explicit string, names map[string]string, id string) string {
	id = strings.TrimSpace(id)
	explicitName := sanitizeAntigravityFunctionName(explicit)
	if matched := names[id]; matched != "" {
		return matched
	}
	if explicitName == "" || explicitName == id {
		return "unknown"
	}
	return explicitName
}

func antigravityChatToGemini(input map[string]any) (map[string]any, error) {
	result := map[string]any{}
	contents := make([]any, 0)
	systemParts := make([]any, 0)
	messages, _ := input["messages"].([]any)
	functionNames := antigravityChatFunctionNames(messages)
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		role, _ := message["role"].(string)
		if role == "system" || role == "developer" {
			systemParts = append(systemParts, antigravityContentParts(message["content"])...)
			continue
		}
		geminiRole := "user"
		if role == "assistant" {
			geminiRole = "model"
		}
		parts := antigravityContentParts(message["content"])
		if reasoning := jsonvalue.FirstString(message, "reasoning_content", "reasoning"); reasoning != "" {
			thought := map[string]any{"text": reasoning, "thought": true}
			if signature := jsonvalue.FirstString(message, "reasoning_signature", "thought_signature"); signature != "" {
				thought["thoughtSignature"] = signature
			}
			parts = append([]any{thought}, parts...)
		}
		if calls, ok := message["tool_calls"].([]any); ok {
			for _, rawCall := range calls {
				call, _ := rawCall.(map[string]any)
				function, _ := call["function"].(map[string]any)
				arguments := map[string]any{}
				switch rawArgs := function["arguments"].(type) {
				case string:
					_ = json.Unmarshal([]byte(rawArgs), &arguments)
				case map[string]any:
					arguments = rawArgs
				}
				parts = append(parts, map[string]any{"functionCall": map[string]any{"name": sanitizeAntigravityFunctionName(jsonvalue.String(function["name"])), "args": arguments, "id": jsonvalue.String(call["id"])}, "thoughtSignature": GeminiSkipThoughtSignature})
			}
		}
		if role == "tool" {
			parts = []any{map[string]any{"functionResponse": map[string]any{"name": antigravityFunctionResponseName(jsonvalue.String(message["name"]), functionNames, jsonvalue.String(message["tool_call_id"])), "id": jsonvalue.String(message["tool_call_id"]), "response": map[string]any{"content": message["content"]}}}}
		}
		contents = append(contents, map[string]any{"role": geminiRole, "parts": parts})
	}
	contents = antigravityMergeConsecutiveRoleContents(contents)
	contents = antigravityAlignFunctionResponses(contents)
	result["contents"] = contents
	if len(systemParts) > 0 {
		result["systemInstruction"] = map[string]any{"parts": systemParts}
	}
	antigravityCopyGenerationConfig(input, result)
	antigravityCopyTools(input, result)
	return result, nil
}

func antigravityResponsesToGemini(input map[string]any, model string) (map[string]any, error) {
	result := map[string]any{}
	functionNames := antigravityResponsesFunctionNameMap(input)
	if instructions, _ := input["instructions"].(string); instructions != "" {
		result["systemInstruction"] = map[string]any{"parts": []any{map[string]any{"text": instructions}}}
	}
	contents := make([]any, 0)
	if text, ok := input["input"].(string); ok {
		contents = append(contents, map[string]any{"role": "user", "parts": []any{map[string]any{"text": text}}})
	} else {
		items := antigravityNormalizeResponsesItems(jsonvalue.Slice(input["input"]))
		callNames := make(map[string]string)
		for index := 0; index < len(items); index++ {
			item, _ := items[index].(map[string]any)
			switch jsonvalue.String(item["type"]) {
			case "function_call", "custom_tool_call":
				callNames[jsonvalue.FirstString(item, "call_id", "id")] = mapAntigravityFunctionName(functionNames, antigravityNamespacedToolName(jsonvalue.String(item["namespace"]), jsonvalue.String(item["name"])))
			}
		}
		for index := 0; index < len(items); index++ {
			item, _ := items[index].(map[string]any)
			switch jsonvalue.String(item["type"]) {
			case "function_call":
				arguments := any(map[string]any{})
				if rawArguments, ok := item["arguments"].(string); ok {
					if err := json.Unmarshal([]byte(rawArguments), &arguments); err != nil {
						arguments = rawArguments
					}
				} else if item["arguments"] != nil {
					arguments = item["arguments"]
				}
				call := map[string]any{"name": mapAntigravityFunctionName(functionNames, antigravityNamespacedToolName(jsonvalue.String(item["namespace"]), jsonvalue.String(item["name"]))), "id": jsonvalue.FirstString(item, "call_id", "id"), "args": arguments}
				contents = append(contents, map[string]any{"role": "model", "parts": []any{map[string]any{"functionCall": call, "thoughtSignature": GeminiSkipThoughtSignature}}})
			case "custom_tool_call":
				call := map[string]any{"name": mapAntigravityFunctionName(functionNames, antigravityNamespacedToolName(jsonvalue.String(item["namespace"]), jsonvalue.String(item["name"]))), "id": jsonvalue.FirstString(item, "call_id", "id"), "args": map[string]any{"input": jsonvalue.String(item["input"])}}
				contents = append(contents, map[string]any{"role": "model", "parts": []any{map[string]any{"functionCall": call, "thoughtSignature": GeminiSkipThoughtSignature}}})
			case "function_call_output", "custom_tool_call_output":
				response := map[string]any{"result": antigravityJSONValue(item["output"])}
				callID := jsonvalue.String(item["call_id"])
				functionResponse := map[string]any{"id": callID, "response": response}
				name := callNames[callID]
				if name == "" {
					name = sanitizeAntigravityFunctionName("unknown")
				}
				functionResponse["name"] = name
				contents = append(contents, map[string]any{"role": "user", "parts": []any{map[string]any{"functionResponse": functionResponse}}})
			case "reasoning":
				visible := ""
				if index+1 < len(items) && strings.Contains(strings.ToLower(model), "gemini") {
					if text, ok := antigravityResponsesAssistantVisibleText(jsonvalue.Map(items[index+1])); ok {
						visible = text
						index++
					}
				}
				parts := antigravityResponsesReasoningParts(item, model, visible)
				if len(parts) > 0 {
					contents = append(contents, map[string]any{"role": "model", "parts": parts})
				}
			default:
				role := strings.ToLower(jsonvalue.String(item["role"]))
				if role == "system" || role == "developer" {
					parts := antigravityContentParts(item["content"])
					if len(parts) > 0 {
						system := jsonvalue.Map(result["systemInstruction"])
						if system == nil {
							system = map[string]any{"parts": []any{}}
						}
						system["parts"] = append(jsonvalue.Slice(system["parts"]), parts...)
						result["systemInstruction"] = system
					}
					continue
				}
				for _, content := range antigravityResponsesMessageContents(item) {
					contents = append(contents, content)
				}
			}
		}
	}
	contents = antigravityMergeConsecutiveRoleContents(contents)
	contents = antigravityAlignFunctionResponses(contents)
	if len(contents) > 0 && antigravityIsTrailingModelPrefill(jsonvalue.Map(contents[len(contents)-1])) {
		contents = contents[:len(contents)-1]
	}
	result["contents"] = contents
	antigravityCopyGenerationConfig(input, result)
	antigravityApplyResponsesGenerationConfig(input, result)
	// CLIProxyAPI's Responses translator carries custom functions and drops
	// Responses built-in tools. Sending web_search alongside functions changes
	// the Gemini tool-combination contract and Antigravity rejects the request.
	antigravityCopyToolsFilteredMapped(input, result, false, functionNames)
	return result, nil
}

func antigravityResponsesMessageContents(item map[string]any) []any {
	defaultRole := "user"
	if role := strings.ToLower(jsonvalue.String(item["role"])); role == "assistant" || role == "model" {
		defaultRole = "model"
	}
	blocks, ok := item["content"].([]any)
	if !ok {
		parts := antigravityContentParts(item["content"])
		if len(parts) == 0 {
			return nil
		}
		return []any{map[string]any{"role": defaultRole, "parts": parts}}
	}
	contents := make([]any, 0)
	currentRole := ""
	currentParts := make([]any, 0)
	flush := func() {
		if len(currentParts) > 0 {
			contents = append(contents, map[string]any{"role": currentRole, "parts": currentParts})
		}
		currentParts = nil
	}
	for _, raw := range blocks {
		block := jsonvalue.Map(raw)
		role := defaultRole
		if jsonvalue.String(block["type"]) == "output_text" {
			role = "model"
		}
		parts := antigravityContentParts([]any{block})
		if len(parts) == 0 {
			continue
		}
		if currentRole != "" && role != currentRole {
			flush()
		}
		currentRole = role
		currentParts = append(currentParts, parts...)
	}
	flush()
	return contents
}

func antigravityResponsesAssistantVisibleText(item map[string]any) (string, bool) {
	itemType := jsonvalue.String(item["type"])
	if itemType != "" && itemType != "message" {
		return "", false
	}
	if text, ok := item["content"].(string); ok {
		role := strings.ToLower(jsonvalue.String(item["role"]))
		return text, role == "assistant" || role == "model"
	}
	texts := make([]string, 0)
	for _, raw := range jsonvalue.Slice(item["content"]) {
		part := jsonvalue.Map(raw)
		if jsonvalue.String(part["type"]) == "output_text" {
			texts = append(texts, jsonvalue.String(part["text"]))
		}
	}
	return strings.Join(texts, "\n"), len(texts) > 0
}

func antigravityResponsesReasoningParts(item map[string]any, model, visible string) []any {
	texts := make([]string, 0)
	for _, raw := range jsonvalue.Slice(item["summary"]) {
		part := jsonvalue.Map(raw)
		if jsonvalue.String(part["type"]) == "summary_text" {
			texts = append(texts, jsonvalue.String(part["text"]))
		}
	}
	if len(texts) == 0 {
		for _, raw := range antigravityContentParts(item["content"]) {
			if text := jsonvalue.String(jsonvalue.Map(raw)["text"]); text != "" {
				texts = append(texts, text)
			}
		}
	}
	thoughtText := strings.Join(texts, "")
	if thoughtText == "" {
		thoughtText = "[reasoning unavailable]"
	}
	signature := normalizeAntigravityGeminiSignature(jsonvalue.FirstString(item, "encrypted_content", "signature"))
	if strings.Contains(strings.ToLower(model), "gemini") {
		return []any{
			map[string]any{"text": thoughtText, "thought": true},
			map[string]any{"text": visible, "thoughtSignature": signature},
		}
	}
	return []any{map[string]any{"text": thoughtText, "thought": true, "thoughtSignature": signature}}
}

func normalizeAntigravityGeminiSignature(signature string) string {
	signature = strings.TrimSpace(signature)
	if strings.HasPrefix(signature, "gemini#") {
		return strings.TrimPrefix(signature, "gemini#")
	}
	if signature == GeminiSkipThoughtSignature {
		return signature
	}
	if strings.HasPrefix(signature, "gpt#") || strings.HasPrefix(signature, "claude#") || signature == "" {
		return GeminiSkipThoughtSignature
	}
	decoded, err := base64.StdEncoding.DecodeString(signature)
	if err == nil && len(decoded) >= 16 && strings.HasPrefix(signature, "E") {
		return signature
	}
	return GeminiSkipThoughtSignature
}

func antigravityIsTrailingModelPrefill(content map[string]any) bool {
	if jsonvalue.String(content["role"]) != "model" {
		return false
	}
	for _, raw := range jsonvalue.Slice(content["parts"]) {
		part := jsonvalue.Map(raw)
		if part["thought"] == true || part["functionCall"] != nil || part["thoughtSignature"] != nil {
			return false
		}
	}
	return true
}

// antigravityEnsureTrailingUserTurn guarantees the request does not end with a
// model content: Gemini rejects those with "Requests ending with a model turn
// are not supported". Plain-text prefill is stripped by the translators, so
// this covers the remaining cases — aborted turns with signed reasoning parts
// and dangling function calls — by appending a continuation user turn.
func antigravityEnsureTrailingUserTurn(contents []any) []any {
	if len(contents) == 0 {
		return contents
	}
	last := jsonvalue.Map(contents[len(contents)-1])
	if last == nil || jsonvalue.String(last["role"]) != "model" {
		return contents
	}
	parts := make([]any, 0)
	for _, raw := range jsonvalue.Slice(last["parts"]) {
		call := jsonvalue.Map(jsonvalue.Map(raw)["functionCall"])
		if call == nil {
			continue
		}
		parts = append(parts, map[string]any{"functionResponse": map[string]any{
			"id":   jsonvalue.String(call["id"]),
			"name": jsonvalue.String(call["name"]),
			"response": map[string]any{
				"error": "tool call was not executed",
			},
		}})
	}
	if len(parts) == 0 {
		parts = []any{map[string]any{"text": "Continue."}}
	}
	return append(contents, map[string]any{"role": "user", "parts": parts})
}

func antigravityNormalizeResponsesItems(items []any) []any {
	normalized := make([]any, 0, len(items))
	for index := 0; index < len(items); {
		item := jsonvalue.Map(items[index])
		if jsonvalue.String(item["type"]) != "function_call" {
			normalized = append(normalized, items[index])
			index++
			continue
		}
		calls := make([]any, 0)
		for index < len(items) && jsonvalue.String(jsonvalue.Map(items[index])["type"]) == "function_call" {
			calls = append(calls, items[index])
			index++
		}
		outputs := make([]any, 0)
		for index < len(items) && jsonvalue.String(jsonvalue.Map(items[index])["type"]) == "function_call_output" {
			outputs = append(outputs, items[index])
			index++
		}
		byCallID := make(map[string]any, len(outputs))
		for _, output := range outputs {
			byCallID[jsonvalue.String(jsonvalue.Map(output)["call_id"])] = output
		}
		for _, call := range calls {
			normalized = append(normalized, call)
			callID := jsonvalue.String(jsonvalue.Map(call)["call_id"])
			if output, ok := byCallID[callID]; ok {
				normalized = append(normalized, output)
				delete(byCallID, callID)
			}
		}
		for _, output := range outputs {
			if _, ok := byCallID[jsonvalue.String(jsonvalue.Map(output)["call_id"])]; ok {
				normalized = append(normalized, output)
			}
		}
	}
	return normalized
}

func antigravityJSONValue(value any) any {
	text, ok := value.(string)
	if !ok {
		return value
	}
	var decoded any
	if json.Unmarshal([]byte(text), &decoded) == nil {
		return decoded
	}
	return text
}

func antigravityAnthropicToGemini(input map[string]any) (map[string]any, error) {
	result := map[string]any{}
	if system := antigravityContentParts(input["system"]); len(system) > 0 {
		result["systemInstruction"] = map[string]any{"parts": system}
	}
	contents := make([]any, 0)
	messages, _ := input["messages"].([]any)
	functionNames := antigravityAnthropicFunctionNames(messages)
	for _, raw := range messages {
		message, _ := raw.(map[string]any)
		role := "user"
		if message["role"] == "assistant" {
			role = "model"
		}
		parts := make([]any, 0)
		switch content := message["content"].(type) {
		case string:
			parts = append(parts, map[string]any{"text": content})
		case []any:
			for _, rawBlock := range content {
				block, _ := rawBlock.(map[string]any)
				switch block["type"] {
				case "text":
					parts = append(parts, map[string]any{"text": jsonvalue.String(block["text"])})
				case "thinking", "redacted_thinking":
					part := map[string]any{"text": jsonvalue.String(block["thinking"]), "thought": true}
					if signature := jsonvalue.String(block["signature"]); signature != "" {
						part["thoughtSignature"] = signature
					}
					parts = append(parts, part)
				case "image", "document":
					source, _ := block["source"].(map[string]any)
					if source["type"] == "base64" {
						parts = append(parts, map[string]any{"inlineData": map[string]any{"mimeType": source["media_type"], "data": source["data"]}})
					} else if sourceURL := jsonvalue.String(source["url"]); sourceURL != "" {
						parts = append(parts, map[string]any{"fileData": map[string]any{"fileUri": sourceURL}})
					}
				case "tool_use":
					parts = append(parts, map[string]any{"functionCall": map[string]any{"name": sanitizeAntigravityFunctionName(jsonvalue.String(block["name"])), "args": block["input"], "id": block["id"]}, "thoughtSignature": GeminiSkipThoughtSignature})
				case "tool_result":
					parts = append(parts, map[string]any{"functionResponse": map[string]any{"name": antigravityFunctionResponseName(jsonvalue.String(block["name"]), functionNames, jsonvalue.String(block["tool_use_id"])), "id": block["tool_use_id"], "response": map[string]any{"content": block["content"], "is_error": block["is_error"]}}})
				}
			}
		}
		contents = append(contents, map[string]any{"role": role, "parts": parts})
	}
	contents = antigravityMergeConsecutiveRoleContents(contents)
	contents = antigravityAlignFunctionResponses(contents)
	result["contents"] = contents
	antigravityCopyGenerationConfig(input, result)
	antigravityCopyTools(input, result)
	return result, nil
}

func antigravityContentParts(value any) []any {
	parts := make([]any, 0)
	switch content := value.(type) {
	case string:
		if content != "" {
			parts = append(parts, map[string]any{"text": content})
		}
	case []any:
		for _, raw := range content {
			block, _ := raw.(map[string]any)
			typeName := jsonvalue.String(block["type"])
			switch typeName {
			case "text", "input_text", "output_text":
				parts = append(parts, map[string]any{"text": jsonvalue.String(block["text"])})
			case "image_url":
				imageURL, _ := block["image_url"].(map[string]any)
				if urlText := jsonvalue.String(imageURL["url"]); strings.HasPrefix(urlText, "data:") {
					if inline := antigravityDataURL(urlText); inline != nil {
						parts = append(parts, inline)
					}
				} else if urlText != "" {
					parts = append(parts, map[string]any{"fileData": map[string]any{"fileUri": urlText}})
				}
			case "input_image":
				urlText := jsonvalue.FirstString(block, "image_url", "url")
				if strings.HasPrefix(urlText, "data:") {
					if inline := antigravityDataURL(urlText); inline != nil {
						parts = append(parts, inline)
					}
				} else if urlText != "" {
					parts = append(parts, map[string]any{"fileData": map[string]any{"fileUri": urlText}})
				}
			case "input_audio":
				audio, _ := block["input_audio"].(map[string]any)
				if audio == nil {
					audio = block
				}
				if data := jsonvalue.String(audio["data"]); data != "" {
					parts = append(parts, map[string]any{"inlineData": map[string]any{"mimeType": antigravityAudioMIME(jsonvalue.String(audio["format"])), "data": data}})
				}
			case "file":
				file, _ := block["file"].(map[string]any)
				if data := jsonvalue.String(file["file_data"]); strings.HasPrefix(data, "data:") {
					if inline := antigravityDataURL(data); inline != nil {
						parts = append(parts, inline)
					}
				} else if fileID := jsonvalue.String(file["file_id"]); fileID != "" {
					parts = append(parts, map[string]any{"fileData": map[string]any{"fileUri": fileID}})
				}
			}
		}
	}
	return parts
}

func antigravityAudioMIME(format string) string {
	if mime := map[string]string{"mp3": "audio/mpeg", "wav": "audio/wav", "ogg": "audio/ogg", "flac": "audio/flac", "aac": "audio/aac", "webm": "audio/webm", "pcm16": "audio/pcm", "g711_ulaw": "audio/basic", "g711_alaw": "audio/basic"}[format]; mime != "" {
		return mime
	}
	if format == "" {
		return "audio/wav"
	}
	return "audio/" + format
}

func antigravityDataURL(value string) map[string]any {
	comma := strings.IndexByte(value, ',')
	if comma < 0 {
		return nil
	}
	header, data := value[5:comma], value[comma+1:]
	mime := strings.TrimSuffix(strings.Split(header, ";")[0], ";base64")
	if _, err := base64.StdEncoding.DecodeString(data); err != nil {
		return nil
	}
	return map[string]any{"inlineData": map[string]any{"mimeType": mime, "data": data}}
}

func antigravityCopyGenerationConfig(input, result map[string]any) {
	config := map[string]any{}
	for source, target := range map[string]string{"temperature": "temperature", "top_p": "topP", "top_k": "topK", "max_tokens": "maxOutputTokens", "max_completion_tokens": "maxOutputTokens", "max_output_tokens": "maxOutputTokens", "stop": "stopSequences"} {
		if value, ok := input[source]; ok {
			config[target] = value
		}
	}
	if responseFormat, ok := input["response_format"].(map[string]any); ok {
		antigravityCopyResponseFormat(responseFormat, config)
	}
	if text, ok := input["text"].(map[string]any); ok {
		if format, ok := text["format"].(map[string]any); ok {
			antigravityCopyResponseFormat(format, config)
		}
	}
	if thinking, ok := input["thinking"].(map[string]any); ok {
		budget := thinking["budget_tokens"]
		if budget == nil {
			budget = thinking["budget"]
		}
		if budget != nil {
			config["thinkingConfig"] = map[string]any{"thinkingBudget": budget, "includeThoughts": true}
		}
	}
	if effort := jsonvalue.String(input["reasoning_effort"]); effort != "" {
		budgets := map[string]int{"none": 0, "minimal": 1024, "low": 4096, "medium": 8192, "high": 24576}
		config["thinkingConfig"] = map[string]any{"thinkingBudget": budgets[effort], "includeThoughts": effort != "none"}
	}
	if reasoning, ok := input["reasoning"].(map[string]any); ok {
		if effort := jsonvalue.String(reasoning["effort"]); effort != "" {
			budgets := map[string]int{"none": 0, "minimal": 1024, "low": 4096, "medium": 8192, "high": 24576}
			config["thinkingConfig"] = map[string]any{"thinkingBudget": budgets[effort], "includeThoughts": effort != "none"}
		}
	}
	if output, ok := input["output_config"].(map[string]any); ok {
		if effort := jsonvalue.String(output["effort"]); effort != "" {
			config["thinkingConfig"] = map[string]any{"thinkingLevel": effort, "includeThoughts": true}
		}
	}
	if len(config) > 0 {
		result["generationConfig"] = config
	}
}

func antigravityCopyResponseFormat(format, config map[string]any) {
	if format["type"] == "json_object" || format["type"] == "json_schema" {
		config["responseMimeType"] = "application/json"
	}
	if schema, ok := format["json_schema"].(map[string]any); ok {
		if inner, ok := schema["schema"].(map[string]any); ok {
			config["responseSchema"] = cleanAntigravitySchema(inner)
		}
	}
	if schema, ok := format["schema"].(map[string]any); ok {
		config["responseSchema"] = cleanAntigravitySchema(schema)
	}
}

func antigravityApplyResponsesGenerationConfig(input, result map[string]any) {
	config := jsonvalue.Map(result["generationConfig"])
	if config == nil {
		config = map[string]any{}
	}
	if text := jsonvalue.Map(input["text"]); text != nil {
		if format := jsonvalue.Map(text["format"]); format != nil {
			switch strings.ToLower(jsonvalue.String(format["type"])) {
			case "json_object":
				config["responseMimeType"] = "application/json"
				delete(config, "responseSchema")
				delete(config, "responseJsonSchema")
			case "json_schema":
				config["responseMimeType"] = "application/json"
				delete(config, "responseSchema")
				schema := jsonvalue.Map(format["schema"])
				if schema == nil {
					schema = jsonvalue.Map(jsonvalue.Map(format["json_schema"])["schema"])
				}
				if schema != nil {
					config["responseJsonSchema"] = schema
				}
			}
		}
	}
	if reasoning := jsonvalue.Map(input["reasoning"]); reasoning != nil {
		if effort := strings.ToLower(strings.TrimSpace(jsonvalue.String(reasoning["effort"]))); effort != "" {
			if effort == "auto" {
				config["thinkingConfig"] = map[string]any{"thinkingBudget": -1, "includeThoughts": true}
			} else {
				config["thinkingConfig"] = map[string]any{"thinkingLevel": effort, "includeThoughts": effort != "none"}
			}
		}
	}
	if len(config) > 0 {
		result["generationConfig"] = config
	}
}

func antigravityCopyTools(input, result map[string]any) {
	antigravityCopyToolsFiltered(input, result, true)
}

func antigravityCopyToolsFiltered(input, result map[string]any, includeBuiltIns bool) {
	antigravityCopyToolsFilteredMapped(input, result, includeBuiltIns, nil)
}

func antigravityCopyToolsFilteredMapped(input, result map[string]any, includeBuiltIns bool, functionNames map[string]string) {
	rawTools, _ := input["tools"].([]any)
	declarations := make([]any, 0)
	googleTools := make([]any, 0)
	seenDeclarations := make(map[string]bool)
	for _, raw := range rawTools {
		tool, _ := raw.(map[string]any)
		toolType := jsonvalue.String(tool["type"])
		if strings.HasPrefix(toolType, "web_search") || tool["google_search"] != nil {
			if includeBuiltIns {
				googleTools = append(googleTools, map[string]any{"googleSearch": map[string]any{}})
			}
			continue
		}
		definition := tool
		if function, ok := tool["function"].(map[string]any); ok {
			definition = function
		}
		name := mapAntigravityFunctionName(functionNames, jsonvalue.String(definition["name"]))
		if name == "" {
			continue
		}
		if seenDeclarations[name] {
			continue
		}
		seenDeclarations[name] = true
		declaration := map[string]any{"name": name}
		if description := strings.TrimSpace(jsonvalue.String(definition["description"])); description != "" {
			declaration["description"] = description
		}
		schema := jsonvalue.Map(definition["input_schema"])
		if schema == nil {
			schema = jsonvalue.Map(definition["parameters"])
		}
		declaration["parameters"] = cleanAntigravityFunctionParameters(schema)
		declarations = append(declarations, declaration)
	}
	tools := make([]any, 0)
	if len(declarations) > 0 {
		tools = append(tools, map[string]any{"functionDeclarations": declarations})
	}
	tools = append(tools, googleTools...)
	if len(tools) > 0 {
		result["tools"] = tools
	}
	toolConfig := map[string]any{}
	if len(googleTools) > 0 && len(declarations) > 0 {
		// Antigravity rejects mixed built-in and function tools unless this
		// native flag explicitly permits server-side tool execution.
		toolConfig["includeServerSideToolInvocations"] = true
	}
	if choice, ok := input["tool_choice"].(map[string]any); ok {
		mode := "AUTO"
		if choice["type"] == "none" {
			mode = "NONE"
		}
		if choice["type"] == "any" || choice["type"] == "required" {
			mode = "ANY"
		}
		if len(googleTools) > 0 && len(declarations) > 0 && mode == "AUTO" {
			mode = "VALIDATED"
		}
		config := map[string]any{"mode": mode}
		name := jsonvalue.String(choice["name"])
		if function, _ := choice["function"].(map[string]any); name == "" && function != nil {
			name = jsonvalue.String(function["name"])
		}
		if name != "" {
			config["allowedFunctionNames"] = []string{mapAntigravityFunctionName(functionNames, name)}
		}
		toolConfig["functionCallingConfig"] = config
	} else if choice, ok := input["tool_choice"].(string); ok {
		mode := "AUTO"
		if choice == "none" {
			mode = "NONE"
		} else if choice == "required" || choice == "any" {
			mode = "ANY"
		}
		if len(googleTools) > 0 && len(declarations) > 0 && mode == "AUTO" {
			mode = "VALIDATED"
		}
		toolConfig["functionCallingConfig"] = map[string]any{"mode": mode}
	}
	if len(toolConfig) > 0 {
		result["toolConfig"] = toolConfig
	}
}

func antigravityResponsesFunctionNameMap(input map[string]any) map[string]string {
	unique := make(map[string]bool)
	baseCounts := make(map[string]int)
	for _, raw := range jsonvalue.Slice(input["tools"]) {
		tool := jsonvalue.Map(raw)
		definition := tool
		if function := jsonvalue.Map(tool["function"]); function != nil {
			definition = function
		}
		name := jsonvalue.String(definition["name"])
		if name == "" || unique[name] {
			continue
		}
		unique[name] = true
		baseCounts[sanitizeAntigravityFunctionName(name)]++
	}
	if len(unique) == 0 {
		return nil
	}
	names := make([]string, 0, len(unique))
	for name := range unique {
		names = append(names, name)
	}
	sort.Strings(names)
	mapped := make(map[string]string, len(names))
	used := make(map[string]bool, len(names))
	for _, name := range names {
		base := sanitizeAntigravityFunctionName(name)
		value := base
		if baseCounts[base] > 1 || used[value] {
			for attempt := 0; ; attempt++ {
				digest := sha256.Sum256([]byte(fmt.Sprintf("%s\x00%d", name, attempt)))
				suffix := "_" + hex.EncodeToString(digest[:6])
				prefix := base
				if len(prefix) > 64-len(suffix) {
					prefix = prefix[:64-len(suffix)]
				}
				value = prefix + suffix
				if !used[value] {
					break
				}
			}
		}
		mapped[name] = value
		used[value] = true
	}
	return mapped
}

func mapAntigravityFunctionName(nameMap map[string]string, name string) string {
	if mapped := nameMap[name]; mapped != "" {
		return mapped
	}
	return sanitizeAntigravityFunctionName(name)
}

func reverseAntigravityFunctionNameMap(nameMap map[string]string) map[string]string {
	if len(nameMap) == 0 {
		return nil
	}
	reversed := make(map[string]string, len(nameMap))
	for original, mapped := range nameMap {
		reversed[mapped] = original
	}
	return reversed
}

// antigravityInlineAdditionalTools flattens Codex 0.155+ `additional_tools`
// input items into top-level function tools so the Antigravity upstream
// receives real tool declarations. Namespaced tools are encoded upstream as
// "<namespace>.<name>" and decoded back into the `namespace` field on
// returned tool calls. Freeform custom tools become functions with a single
// string `input` parameter and are returned as `custom_tool_call` items. It
// returns the set of namespaced tool names and the subset that are freeform
// custom tools.
func antigravityInlineAdditionalTools(root map[string]any) (namespaced, custom map[string]bool) {
	items := jsonvalue.Slice(root["input"])
	if len(items) == 0 {
		return nil, nil
	}
	namespaced = map[string]bool{}
	custom = map[string]bool{}
	flattened := make([]any, 0, 8)
	kept := make([]any, 0, len(items))
	changed := false
	for _, raw := range items {
		item := jsonvalue.Map(raw)
		if item == nil || jsonvalue.String(item["type"]) != "additional_tools" {
			kept = append(kept, raw)
			continue
		}
		changed = true
		flattened = append(flattened, antigravityFlattenToolDeclarations(jsonvalue.Slice(item["tools"]), "", namespaced, custom)...)
	}
	if !changed {
		return nil, nil
	}
	if len(flattened) > 0 {
		root["tools"] = append(jsonvalue.Slice(root["tools"]), flattened...)
	}
	root["input"] = kept
	return namespaced, custom
}

func antigravityFlattenToolDeclarations(tools []any, namespace string, namespaced, custom map[string]bool) []any {
	flattened := make([]any, 0, len(tools))
	for _, raw := range tools {
		tool := jsonvalue.Map(raw)
		if tool == nil {
			continue
		}
		switch jsonvalue.String(tool["type"]) {
		case "namespace":
			name := sanitizeAntigravityNamespaceComponent(jsonvalue.String(tool["name"]))
			if name == "" {
				continue
			}
			if namespace != "" {
				name = namespace + "_" + name
			}
			flattened = append(flattened, antigravityFlattenToolDeclarations(jsonvalue.Slice(tool["tools"]), name, namespaced, custom)...)
		case "function":
			definition := tool
			if function := jsonvalue.Map(tool["function"]); function != nil {
				definition = function
			}
			name := antigravityNamespacedToolName(namespace, jsonvalue.String(definition["name"]))
			if name == "" {
				continue
			}
			if namespace != "" {
				namespaced[name] = true
			}
			converted := map[string]any{"type": "function", "name": name}
			if description := strings.TrimSpace(jsonvalue.String(definition["description"])); description != "" {
				converted["description"] = description
			}
			if parameters := jsonvalue.Map(definition["parameters"]); parameters != nil {
				converted["parameters"] = parameters
			} else if parameters := jsonvalue.Map(definition["input_schema"]); parameters != nil {
				converted["parameters"] = parameters
			}
			flattened = append(flattened, converted)
		case "custom":
			name := antigravityNamespacedToolName(namespace, jsonvalue.String(tool["name"]))
			if name == "" {
				continue
			}
			if namespace != "" {
				namespaced[name] = true
			}
			custom[name] = true
			description := strings.TrimSpace(jsonvalue.String(tool["description"]))
			if description == "" {
				description = "Freeform tool."
			}
			description += "\n\nThis is a FREEFORM tool: provide the raw tool input as the `input` string, not JSON."
			flattened = append(flattened, map[string]any{
				"type":        "function",
				"name":        name,
				"description": description,
				"parameters": map[string]any{
					"type":       "object",
					"properties": map[string]any{"input": map[string]any{"type": "string", "description": "Raw freeform tool input."}},
					"required":   []any{"input"},
				},
			})
		}
	}
	return flattened
}

// antigravityNamespacedToolName encodes a client tool name and optional
// namespace as the upstream function name.
func antigravityNamespacedToolName(namespace, name string) string {
	name = strings.TrimSpace(name)
	if name == "" {
		return ""
	}
	if namespace = strings.TrimSpace(namespace); namespace == "" {
		return name
	}
	return namespace + "." + name
}

func sanitizeAntigravityNamespaceComponent(namespace string) string {
	var builder strings.Builder
	for _, char := range strings.TrimSpace(namespace) {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '_' || char == '-' {
			builder.WriteRune(char)
		} else {
			builder.WriteByte('_')
		}
	}
	return builder.String()
}

func cleanAntigravityFunctionParameters(schema map[string]any) map[string]any {
	if schema == nil {
		return map[string]any{"type": "object", "properties": map[string]any{}}
	}
	cleaned := cleanAntigravitySchema(schema)
	cleaned["type"] = "object"
	if properties, ok := cleaned["properties"].(map[string]any); !ok || properties == nil {
		cleaned["properties"] = map[string]any{}
	}
	return cleaned
}

func cleanAntigravitySchema(schema map[string]any) map[string]any {
	return cleanAntigravitySchemaNode(schema, false)
}

var antigravityUnsupportedSchemaKeywords = map[string]bool{
	"$schema": true, "$defs": true, "definitions": true, "$id": true, "additionalProperties": true,
	"propertyNames": true, "patternProperties": true, "$comment": true, "enumDescriptions": true,
	"enumTitles": true, "prefill": true, "deprecated": true, "strict": true, "nullable": true,
	"title": true, "minLength": true, "maxLength": true, "exclusiveMinimum": true,
	"exclusiveMaximum": true, "pattern": true, "minItems": true, "maxItems": true,
	"uniqueItems": true, "format": true, "default": true, "examples": true,
	"encrypted": true,
}

var antigravitySchemaConstraintHints = map[string]string{
	"minLength": "minLength", "maxLength": "maxLength", "exclusiveMinimum": "exclusiveMinimum",
	"exclusiveMaximum": "exclusiveMaximum", "pattern": "pattern", "minItems": "minItems",
	"maxItems": "maxItems", "uniqueItems": "uniqueItems", "format": "format", "default": "default",
	"examples": "examples",
}

func cleanAntigravitySchemaNode(schema map[string]any, propertyMap bool) map[string]any {
	if propertyMap {
		result := make(map[string]any, len(schema))
		for name, value := range schema {
			if child, ok := value.(map[string]any); ok {
				result[name] = cleanAntigravitySchemaNode(child, false)
			} else {
				result[name] = value
			}
		}
		return result
	}

	working := cloneMap(schema)
	nullableProperties := make(map[string]bool)
	if properties, ok := working["properties"].(map[string]any); ok {
		for name, raw := range properties {
			if child, ok := raw.(map[string]any); ok && antigravitySchemaAllowsNull(child) {
				nullableProperties[name] = true
			}
		}
	}
	if ref := jsonvalue.String(working["$ref"]); ref != "" {
		name := ref
		if index := strings.LastIndexByte(ref, '/'); index >= 0 {
			name = ref[index+1:]
		}
		description := antigravityAppendSchemaHint(jsonvalue.String(working["description"]), "See: "+name)
		return map[string]any{"type": "object", "description": description}
	}

	antigravityMergeAllOf(working)
	working = antigravityFlattenSchemaUnion(working)

	result := make(map[string]any, len(working))
	description := jsonvalue.String(working["description"])
	for key, value := range working {
		// Gemini rejects a null "required" outright, so drop the key instead of
		// replacing it with an empty list as the OpenAI-family paths do.
		if key == "required" && value == nil {
			continue
		}
		if key == "description" || key == "const" || key == "allOf" || key == "anyOf" || key == "oneOf" || key == "$ref" {
			continue
		}
		if hint, ok := antigravitySchemaConstraintHints[key]; ok {
			description = antigravityAppendSchemaHint(description, fmt.Sprintf("%s: %v", hint, value))
			continue
		}
		if antigravityUnsupportedSchemaKeywords[key] || strings.HasPrefix(key, "x-") {
			if key == "additionalProperties" && value == false {
				description = antigravityAppendSchemaHint(description, "No extra properties allowed")
			}
			continue
		}
		switch key {
		case "properties":
			if properties, ok := value.(map[string]any); ok {
				result[key] = cleanAntigravitySchemaNode(properties, true)
			}
		case "items":
			if child, ok := value.(map[string]any); ok {
				result[key] = cleanAntigravitySchemaNode(child, false)
			} else {
				result[key] = value
			}
		case "type":
			if types, ok := value.([]any); ok {
				selected := "string"
				accepted := make([]string, 0, len(types))
				for _, rawType := range types {
					typeName := jsonvalue.String(rawType)
					if typeName == "" || typeName == "null" {
						continue
					}
					if len(accepted) == 0 {
						selected = typeName
					}
					accepted = append(accepted, typeName)
				}
				result[key] = selected
				if len(accepted) > 1 {
					description = antigravityAppendSchemaHint(description, "Accepts: "+strings.Join(accepted, " | "))
				}
				if len(types) != len(accepted) {
					description = antigravityAppendSchemaHint(description, "(nullable)")
				}
			} else {
				result[key] = value
			}
		default:
			result[key] = value
		}
	}

	if constant, exists := working["const"]; exists {
		if _, hasEnum := result["enum"]; !hasEnum {
			result["enum"] = []any{constant}
		}
	}
	if values, ok := result["enum"].([]any); ok {
		converted := make([]any, 0, len(values))
		for _, value := range values {
			converted = append(converted, fmt.Sprint(value))
		}
		result["enum"] = converted
		result["type"] = "string"
		if len(converted) > 1 && len(converted) <= 10 {
			labels := make([]string, len(converted))
			for i, value := range converted {
				labels[i] = fmt.Sprint(value)
			}
			description = antigravityAppendSchemaHint(description, "Allowed: "+strings.Join(labels, ", "))
		}
	}
	if required, ok := result["required"].([]any); ok {
		properties, _ := result["properties"].(map[string]any)
		cleaned := make([]any, 0, len(required))
		for _, value := range required {
			name := jsonvalue.String(value)
			if _, exists := properties[name]; exists && !nullableProperties[name] {
				cleaned = append(cleaned, name)
			}
		}
		if len(cleaned) == 0 {
			delete(result, "required")
		} else {
			result["required"] = cleaned
		}
	}
	if description != "" {
		result["description"] = description
	}
	return result
}

func antigravitySchemaAllowsNull(schema map[string]any) bool {
	if types, ok := schema["type"].([]any); ok {
		for _, value := range types {
			if jsonvalue.String(value) == "null" {
				return true
			}
		}
	}
	for _, keyword := range []string{"anyOf", "oneOf"} {
		for _, raw := range jsonvalue.Slice(schema[keyword]) {
			if child, ok := raw.(map[string]any); ok && jsonvalue.String(child["type"]) == "null" {
				return true
			}
		}
	}
	return false
}

func antigravityMergeAllOf(schema map[string]any) {
	allOf, _ := schema["allOf"].([]any)
	properties, _ := schema["properties"].(map[string]any)
	if properties == nil {
		properties = make(map[string]any)
	}
	required, _ := schema["required"].([]any)
	seenRequired := make(map[string]bool)
	for _, value := range required {
		seenRequired[jsonvalue.String(value)] = true
	}
	for _, raw := range allOf {
		item, _ := raw.(map[string]any)
		for name, value := range jsonvalue.Map(item["properties"]) {
			properties[name] = value
		}
		for _, value := range jsonvalue.Slice(item["required"]) {
			name := jsonvalue.String(value)
			if name != "" && !seenRequired[name] {
				required = append(required, name)
				seenRequired[name] = true
			}
		}
	}
	if len(properties) > 0 {
		schema["properties"] = properties
	}
	if len(required) > 0 {
		schema["required"] = required
	}
	delete(schema, "allOf")
}

func antigravityFlattenSchemaUnion(schema map[string]any) map[string]any {
	for _, keyword := range []string{"anyOf", "oneOf"} {
		options, _ := schema[keyword].([]any)
		if len(options) == 0 {
			continue
		}
		bestScore, best := -1, map[string]any{}
		types := make([]string, 0, len(options))
		for _, raw := range options {
			option, _ := raw.(map[string]any)
			typeName := jsonvalue.String(option["type"])
			score := 0
			switch {
			case typeName == "object" || option["properties"] != nil:
				score = 3
				typeName = "object"
			case typeName == "array" || option["items"] != nil:
				score = 2
				typeName = "array"
			case typeName != "" && typeName != "null":
				score = 1
			default:
				typeName = "null"
			}
			types = append(types, typeName)
			if score > bestScore {
				bestScore, best = score, option
			}
		}
		selected := cloneMap(best)
		if description := jsonvalue.String(schema["description"]); description != "" {
			selected["description"] = antigravityAppendSchemaHint(jsonvalue.String(selected["description"]), description)
		}
		if len(types) > 1 {
			selected["description"] = antigravityAppendSchemaHint(jsonvalue.String(selected["description"]), "Accepts: "+strings.Join(types, " | "))
		}
		for key, value := range schema {
			if key != keyword && key != "description" {
				if _, exists := selected[key]; !exists {
					selected[key] = value
				}
			}
		}
		return selected
	}
	return schema
}

func antigravityAppendSchemaHint(description, hint string) string {
	if description == "" {
		return hint
	}
	if hint == "" || strings.Contains(description, hint) {
		return description
	}
	return description + " (" + hint + ")"
}

func sanitizeAntigravityFunctionName(name string) string {
	if name == "" {
		return ""
	}
	var builder strings.Builder
	for _, char := range name {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '_' || char == '-' || char == '.' || char == ':' {
			builder.WriteRune(char)
		} else {
			builder.WriteByte('_')
		}
	}
	value := builder.String()
	if value == "" {
		value = "_"
	}
	first := value[0]
	if !((first >= 'a' && first <= 'z') || (first >= 'A' && first <= 'Z') || first == '_') {
		if len(value) >= 64 {
			value = value[:63]
		}
		value = "_" + value
	}
	if len(value) > 64 {
		value = value[:64]
	}
	return value
}
