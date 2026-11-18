package main

import (
	"bufio"
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"log"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strconv"
	"strings"
	"sync/atomic"
	"time"

	"github.com/google/uuid"

	"codex-pool-proxy/internal/jsonvalue"
	"codex-pool-proxy/internal/wire"
)

// The Antigravity wire contract and translation behavior are adapted from
// router-for-me/CLIProxyAPI at c8803713c972af0076f55933fdeed4db81d72d24.
// CLIProxyAPI is MIT licensed; see THIRD_PARTY_NOTICES.md.

const antigravityFunctionThoughtSignature = "skip_thought_signature_validator"

type antigravityPreparedRequest struct {
	Body                     []byte
	Format                   wire.RequestFormat
	ClientStream             bool
	PublicModel              string
	UpstreamModel            string
	Operation                string
	ResponsesRequest         map[string]any
	ResponsesFunctionNames   map[string]string
	ResponsesNamespacedTools map[string]bool
	ResponsesCustomTools     map[string]bool
	EstimatedInputTokens     int
	Provider                 AccountType // upstream account type; empty means Antigravity
}

func (prepared antigravityPreparedRequest) provider() AccountType {
	if prepared.Provider == "" {
		return AccountTypeAntigravity
	}
	return prepared.Provider
}

func shouldRouteAntigravityModel(model string) bool {
	model = strings.TrimSpace(model)
	if strings.HasPrefix(model, "antigravity/") {
		return true
	}
	for _, provider := range []AccountType{AccountTypeCodex, AccountTypeClaude, AccountTypeKimi, AccountTypeMinimax, AccountTypeZAI, AccountTypeXiaomi, AccountTypeAdverserial} {
		if _, ok := modelForProvider(provider, model); ok {
			return false
		}
	}
	if isGrokModel(model) {
		return false
	}
	return isAntigravityModel(model)
}

func antigravityModelFromGeminiPath(path string) string {
	marker := "/v1beta/models/"
	index := strings.Index(path, marker)
	if index < 0 {
		return ""
	}
	model := path[index+len(marker):]
	if colon := strings.IndexByte(model, ':'); colon >= 0 {
		model = model[:colon]
	}
	return strings.TrimSpace(model)
}

// unwrapGeminiClientRequest accepts both a raw generateContent body and the
// Gemini CLI / Code Assist envelope {model, request:{contents...}}. Nesting
// the envelope again would send request.request to Google.
func unwrapGeminiClientRequest(root map[string]any) map[string]any {
	request := jsonvalue.Map(root["request"])
	if request != nil {
		if _, ok := request["contents"]; ok {
			return cloneAnyMap(request)
		}
	}
	return cloneAnyMap(root)
}

func prepareAntigravityRequest(path string, body []byte, requestedModel, projectID, conversationID string) (antigravityPreparedRequest, error) {
	publicModel := strings.TrimSpace(requestedModel)
	if publicModel == "" {
		publicModel = antigravityModelFromGeminiPath(path)
	}
	if publicModel == "" {
		return antigravityPreparedRequest{}, errors.New("Antigravity model is required")
	}
	if strings.TrimSpace(projectID) == "" {
		return antigravityPreparedRequest{}, errors.New("Antigravity project ID is required")
	}
	upstreamModel := antigravityCanonicalModel(publicModel)
	return prepareGeminiNativeRequest(path, body, publicModel, upstreamModel, conversationID, func(gemini map[string]any) map[string]any {
		if !strings.Contains(strings.ToLower(upstreamModel), "claude") {
			if generationConfig := jsonvalue.Map(gemini["generationConfig"]); generationConfig != nil {
				delete(generationConfig, "maxOutputTokens")
			}
		}
		requestType := "agent"
		if strings.Contains(strings.ToLower(upstreamModel), "image") {
			requestType = "image_gen"
		}
		requestID := "agent-" + uuid.NewString()
		if requestType == "image_gen" {
			requestID = fmt.Sprintf("image_gen/%d/%s/12", time.Now().UnixMilli(), uuid.NewString())
		}
		return map[string]any{
			"model": upstreamModel, "userAgent": "antigravity", "requestType": requestType,
			"project": projectID, "requestId": requestID, "request": gemini,
		}
	})
}

// prepareGeminiNativeRequest translates a client request (Gemini, Chat
// Completions, Anthropic Messages or Responses) into a native Gemini
// generateContent request. envelope wraps it the way the upstream expects; the
// wrapped body keeps "model" and "request" so the signature replay cache can
// scope it.
func prepareGeminiNativeRequest(path string, body []byte, publicModel, upstreamModel, conversationID string, envelope func(map[string]any) map[string]any) (antigravityPreparedRequest, error) {
	format := wire.FormatGemini
	switch {
	case strings.HasPrefix(path, "/v1/messages"):
		format = wire.FormatClaude
	case isAntigravityResponsesPath(path):
		format = wire.FormatResponses
	case strings.HasPrefix(path, "/v1/chat/completions"):
		format = wire.FormatOpenAI
	}
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return antigravityPreparedRequest{}, fmt.Errorf("invalid JSON request: %w", err)
	}
	var namespacedTools, customTools map[string]bool
	if format == wire.FormatResponses {
		namespacedTools, customTools = antigravityInlineAdditionalTools(root)
	}
	clientStream, _ := root["stream"].(bool)
	operation := "streamGenerateContent"
	if strings.Contains(path, "countTokens") {
		operation = "countTokens"
		clientStream = false
	}
	var gemini map[string]any
	var err error
	switch format {
	case wire.FormatGemini:
		// Gemini CLI / Code Assist sends {model, request:{contents...}}.
		// A v1beta body already has contents at the top level.
		gemini = unwrapGeminiClientRequest(root)
		delete(gemini, "model")
		clientStream = strings.Contains(path, "streamGenerateContent") || strings.HasPrefix(path, "/v1internal:") || clientStream
	case wire.FormatOpenAI:
		gemini, err = antigravityChatToGemini(root)
	case wire.FormatClaude:
		gemini, err = antigravityAnthropicToGemini(root)
	case wire.FormatResponses:
		gemini, err = antigravityResponsesToGemini(root, upstreamModel)
	}
	if err != nil {
		return antigravityPreparedRequest{}, err
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
	gemini["sessionId"] = antigravityFormatSessionID(conversationID, gemini)
	encoded, err := json.Marshal(envelope(gemini))
	var responsesRequest map[string]any
	var responsesFunctionNames map[string]string
	if format == wire.FormatResponses {
		responsesRequest = cloneAnyMap(root)
		responsesFunctionNames = reverseAntigravityFunctionNameMap(antigravityResponsesFunctionNameMap(root))
	}
	return antigravityPreparedRequest{Body: encoded, Format: format, ClientStream: clientStream, PublicModel: publicModel, UpstreamModel: upstreamModel, Operation: operation, ResponsesRequest: responsesRequest, ResponsesFunctionNames: responsesFunctionNames, ResponsesNamespacedTools: namespacedTools, ResponsesCustomTools: customTools}, err
}

func antigravitySessionID(request map[string]any) string {
	seed := ""
	if contents, ok := request["contents"].([]any); ok {
		for _, content := range contents {
			entry, _ := content.(map[string]any)
			if entry["role"] != "user" {
				continue
			}
			parts, _ := entry["parts"].([]any)
			for _, part := range parts {
				if text, _ := part.(map[string]any)["text"].(string); text != "" {
					seed = text
					break
				}
			}
			if seed != "" {
				break
			}
		}
	}
	if seed == "" {
		seed = uuid.NewString()
	}
	return antigravitySessionIDFromSeed(seed)
}

func isAntigravitySessionID(id string) bool {
	id = strings.TrimSpace(id)
	if !strings.HasPrefix(id, "-") {
		return false
	}
	val, err := strconv.ParseInt(id, 10, 64)
	return err == nil && val < 0
}

func antigravitySessionIDFromSeed(seed string) string {
	sum := sha256.Sum256([]byte(seed))
	value := int64(0)
	for _, b := range sum[:8] {
		value = value<<8 | int64(b)
	}
	if value > 0 {
		value = -value
	}
	if value == 0 {
		value = -1
	}
	return strconv.FormatInt(value, 10)
}

func antigravityFormatSessionID(conversationID string, request map[string]any) string {
	conversationID = strings.TrimSpace(conversationID)
	if isAntigravitySessionID(conversationID) {
		return conversationID
	}
	if conversationID != "" {
		return antigravitySessionIDFromSeed(conversationID)
	}
	return antigravitySessionID(request)
}

func cloneAnyMap(input map[string]any) map[string]any {
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
			part := cloneAnyMap(jsonvalue.Map(parts[responseParts[responseIndex]]))
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
		if reasoning := firstAntigravityString(message, "reasoning_content", "reasoning"); reasoning != "" {
			thought := map[string]any{"text": reasoning, "thought": true}
			if signature := firstAntigravityString(message, "reasoning_signature", "thought_signature"); signature != "" {
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
				parts = append(parts, map[string]any{"functionCall": map[string]any{"name": sanitizeAntigravityFunctionName(jsonvalue.String(function["name"])), "args": arguments, "id": jsonvalue.String(call["id"])}, "thoughtSignature": antigravityFunctionThoughtSignature})
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
				callNames[firstAntigravityString(item, "call_id", "id")] = mapAntigravityFunctionName(functionNames, antigravityNamespacedToolName(jsonvalue.String(item["namespace"]), jsonvalue.String(item["name"])))
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
				call := map[string]any{"name": mapAntigravityFunctionName(functionNames, antigravityNamespacedToolName(jsonvalue.String(item["namespace"]), jsonvalue.String(item["name"]))), "id": firstAntigravityString(item, "call_id", "id"), "args": arguments}
				contents = append(contents, map[string]any{"role": "model", "parts": []any{map[string]any{"functionCall": call, "thoughtSignature": antigravityFunctionThoughtSignature}}})
			case "custom_tool_call":
				call := map[string]any{"name": mapAntigravityFunctionName(functionNames, antigravityNamespacedToolName(jsonvalue.String(item["namespace"]), jsonvalue.String(item["name"]))), "id": firstAntigravityString(item, "call_id", "id"), "args": map[string]any{"input": jsonvalue.String(item["input"])}}
				contents = append(contents, map[string]any{"role": "model", "parts": []any{map[string]any{"functionCall": call, "thoughtSignature": antigravityFunctionThoughtSignature}}})
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
	signature := normalizeAntigravityGeminiSignature(firstAntigravityString(item, "encrypted_content", "signature"))
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
	if signature == antigravityFunctionThoughtSignature {
		return signature
	}
	if strings.HasPrefix(signature, "gpt#") || strings.HasPrefix(signature, "claude#") || signature == "" {
		return antigravityFunctionThoughtSignature
	}
	decoded, err := base64.StdEncoding.DecodeString(signature)
	if err == nil && len(decoded) >= 16 && strings.HasPrefix(signature, "E") {
		return signature
	}
	return antigravityFunctionThoughtSignature
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

func firstAntigravityString(input map[string]any, keys ...string) string {
	for _, key := range keys {
		if value, _ := input[key].(string); value != "" {
			return value
		}
	}
	return ""
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
					parts = append(parts, map[string]any{"functionCall": map[string]any{"name": sanitizeAntigravityFunctionName(jsonvalue.String(block["name"])), "args": block["input"], "id": block["id"]}, "thoughtSignature": antigravityFunctionThoughtSignature})
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
				urlText := firstAntigravityString(block, "image_url", "url")
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

// antigravitySplitToolCallName splits a resolved tool name that came from an
// `additional_tools` namespace back into the bare tool name and namespace.
func antigravitySplitToolCallName(name string, namespaced map[string]bool) (bare, namespace string) {
	if len(namespaced) == 0 || !namespaced[name] {
		return name, ""
	}
	if idx := strings.Index(name, "."); idx > 0 {
		return name[idx+1:], name[:idx]
	}
	return name, ""
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

	working := cloneAnyMap(schema)
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
		selected := cloneAnyMap(best)
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

func (h *proxyHandler) handleAntigravityProxy(w http.ResponseWriter, r *http.Request, body []byte, requestedModel, conversationID, userID, originID, clientIP, reqID string, routingProfile RoutingProfile) bool {
	if !shouldRouteAntigravityModel(requestedModel) {
		return false
	}
	canonical := antigravityCanonicalModel(requestedModel)
	provider, _ := h.registry.ForType(AccountTypeAntigravity).(*AntigravityProvider)
	if provider == nil {
		respondJSONError(w, http.StatusServiceUnavailable, "Antigravity provider is not configured")
		return true
	}
	exclude := make(map[string]bool)
	var lastError error
	var lastRateLimitBody []byte
	var lastRateLimitUntil time.Time
	// The last account-specific failure (unrecoverable 401, 5xx) is relayed
	// when every account fails, instead of a generic capacity error.
	var lastFailureStatus int
	var lastFailureBody []byte
	serverFailures := 0
	attempts := h.cfg.maxAttempts
	if attempts < 1 {
		attempts = 1
	}
	if accountCount := h.pool.countByType(AccountTypeAntigravity); accountCount > attempts {
		attempts = accountCount
	}
	contextCompactionRetry := false
	for attempt := 0; attempt < attempts; attempt++ {
		account, policy, reasons, score, alternatives, breakdownView := h.pool.candidateForAntigravityModelWithRoutingTraceForUser(userID, conversationID, exclude, canonical, clientIP, routingProfile)
		if account == nil {
			if err := h.checkAccountRoutingAccess(userID, AccountTypeAntigravity, canonical); err != nil {
				respondPolicyError(w, err)
				return true
			}
			break
		}
		primaryReason := "quota_headroom"
		if len(reasons) > 0 {
			primaryReason = reasons[len(reasons)-1]
		}
		trace := &RouteTrace{
			RequestID:      reqID,
			Transition:     h.transitionForTrace(userID, conversationID, AccountTypeAntigravity, w.Header()),
			Timestamp:      time.Now().UTC(),
			Policy:         policy,
			Selected:       RouteTarget{Provider: "antigravity", Model: canonical},
			Score:          score,
			Reasons:        reasons,
			Alternatives:   alternatives,
			AccountID:      account.ID,
			ScoreBreakdown: breakdownView,
			ClientIP:       clientIP,
			UserID:         userID,
			Attempts:       attempt + 1,
		}
		h.getRouteTraces().Record(trace)
		setPoolRouteHeaders(w.Header(), "antigravity", canonical, policy, primaryReason, attempt+1, reqID)
		exclude[account.ID] = true
		if h.needsRefresh(account) {
			_ = h.refreshAccount(r.Context(), account)
		}
		account.mu.Lock()
		projectID := account.ProjectID
		account.mu.Unlock()
		nativeSeed := conversationID
		freshSession := false
		if conversationID != "" {
			nativeSeed, freshSession = h.getContextHandoff().NativeSessionSeed(conversationScopedKey(userID, conversationID), AccountTypeAntigravity)
		}
		prepared, err := prepareAntigravityRequest(r.URL.Path, body, requestedModel, projectID, nativeSeed)
		if err != nil {
			respondJSONError(w, http.StatusBadRequest, err.Error())
			return true
		}
		var replayScope antigravityReplayScope
		var transitionEpoch uint64
		if conversationID != "" {
			state, _ := h.getContextHandoff().State(conversationScopedKey(userID, conversationID))
			transitionEpoch = state.TransitionEpoch
		}
		prepared.Body, replayScope, _ = antigravityScopedReplay(prepared.Body, conversationID, transitionEpoch, freshSession)
		prepared.EstimatedInputTokens = estimateAntigravitySerializedTokens(prepared.Body)
		if conversationID != "" {
			var envelope map[string]any
			if json.Unmarshal(prepared.Body, &envelope) == nil {
				request, _ := envelope["request"].(map[string]any)
				h.getContextHandoff().BindNativeSession(conversationScopedKey(userID, conversationID), AccountTypeAntigravity, jsonvalue.String(request["sessionId"]))
			}
		}
		attemptStarted := time.Now()
		atomic.AddInt64(&account.Inflight, 1)
		resp, err := h.doAntigravityRequestWithTransientRetry(r.Context(), r.Header, account, provider, prepared)
		atomic.AddInt64(&account.Inflight, -1)
		if err != nil {
			h.pool.recordRoutingOutcome(account.ID, time.Since(attemptStarted), 0, http.StatusServiceUnavailable, time.Now())
			lastError = err
			continue
		}
		trace.DurationMs = float64(time.Since(attemptStarted).Milliseconds())
		trace.StatusCode = resp.StatusCode
		if trace.Transition != nil {
			h.getContextHandoff().MarkTransitionOutcome(conversationScopedKey(userID, conversationID), trace.Transition.Epoch, resp.StatusCode, "", false)
			trace.Transition.StatusCode = resp.StatusCode
		}
		h.getRouteTraces().Record(trace)
		h.pool.recordRoutingOutcome(account.ID, time.Since(attemptStarted), 0, resp.StatusCode, time.Now())
		if resp.StatusCode == http.StatusTooManyRequests && freshSession && conversationID != "" {
			errBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			_ = resp.Body.Close()
			if readErr != nil {
				antigravityWriteError(w, prepared.Format, http.StatusBadGateway, []byte(readErr.Error()))
				return true
			}
			resp.Body = io.NopCloser(bytes.NewReader(errBody))
			resp.ContentLength = int64(len(errBody))
			providerErr := classifyAntigravityError(resp.StatusCode, errBody)
			if trace.Transition != nil {
				trace.Transition.ErrorClass = providerErr.Class
				h.getContextHandoff().MarkTransitionOutcome(conversationScopedKey(userID, conversationID), trace.Transition.Epoch, resp.StatusCode, providerErr.Class, false)
			}
			state, ok := h.getContextHandoff().State(conversationScopedKey(userID, conversationID))
			if ok && (providerErr.Class == ProviderErrorSession || providerErr.Class == ProviderErrorContext || providerErr.Class == ProviderErrorUnknown) {
				transition := &TransitionAttempt{
					Owner: userID, ConversationID: conversationID, Epoch: state.TransitionEpoch,
					From: state.LastTransitionFrom, To: AccountTypeAntigravity,
				}
				if h.getContextHandoff().RecoverNativeSession(transition) {
					if trace.Transition != nil {
						trace.Transition.Epoch = transition.Epoch
						trace.Transition.RecoveryRetry = true
					}
					_ = resp.Body.Close()
					nativeSeed, _ = h.getContextHandoff().NativeSessionSeed(conversationScopedKey(userID, conversationID), AccountTypeAntigravity)
					prepared, err = prepareAntigravityRequest(r.URL.Path, body, requestedModel, projectID, nativeSeed)
					if err != nil {
						antigravityWriteError(w, antigravityFormatForPath(r.URL.Path), http.StatusBadRequest, []byte(err.Error()))
						return true
					}
					prepared.Body, replayScope, _ = antigravityScopedReplay(prepared.Body, conversationID, transition.Epoch, true)
					prepared.EstimatedInputTokens = estimateAntigravitySerializedTokens(prepared.Body)
					var envelope map[string]any
					if json.Unmarshal(prepared.Body, &envelope) == nil {
						request, _ := envelope["request"].(map[string]any)
						h.getContextHandoff().BindNativeSession(conversationScopedKey(userID, conversationID), AccountTypeAntigravity, jsonvalue.String(request["sessionId"]))
					}
					log.Printf("conversation=%s transition=%s->antigravity epoch=%d recovery_retry=true error_class=%s", conversationID, transition.From, transition.Epoch, providerErr.Class)
					atomic.AddInt64(&account.Inflight, 1)
					resp, err = h.doAntigravityRequestWithTransientRetry(r.Context(), r.Header, account, provider, prepared)
					atomic.AddInt64(&account.Inflight, -1)
					if err != nil {
						lastError = err
						continue
					}
					trace.StatusCode = resp.StatusCode
					if trace.Transition != nil {
						trace.Transition.StatusCode = resp.StatusCode
						h.getContextHandoff().MarkTransitionOutcome(conversationScopedKey(userID, conversationID), transition.Epoch, resp.StatusCode, "", true)
					}
					h.getRouteTraces().Record(trace)
					h.pool.recordRoutingOutcome(account.ID, time.Since(attemptStarted), 0, resp.StatusCode, time.Now())
				}
			}
		}
		if resp.StatusCode == http.StatusUnauthorized {
			errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			_ = resp.Body.Close()
			if err := h.refreshAccountAfterAuthFailure(r.Context(), account); err != nil {
				// This credential cannot serve the request; another account may.
				lastFailureStatus, lastFailureBody = http.StatusUnauthorized, errBody
				lastError = fmt.Errorf("Antigravity account %s rejected its credentials: %v", account.ID, err)
				continue
			}
			atomic.AddInt64(&account.Inflight, 1)
			resp, err = h.doAntigravityRequestWithTransientRetry(r.Context(), r.Header, account, provider, prepared)
			atomic.AddInt64(&account.Inflight, -1)
			if err != nil {
				lastError = err
				continue
			}
			if resp.StatusCode == http.StatusUnauthorized {
				errBody, _ = io.ReadAll(io.LimitReader(resp.Body, 1<<20))
				_ = resp.Body.Close()
				lastFailureStatus, lastFailureBody = http.StatusUnauthorized, errBody
				lastError = fmt.Errorf("Antigravity account %s rejected refreshed credentials", account.ID)
				continue
			}
		}
		if resp.StatusCode == http.StatusBadRequest && !contextCompactionRetry {
			errBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			_ = resp.Body.Close()
			if readErr != nil {
				antigravityWriteError(w, prepared.Format, http.StatusBadGateway, []byte(readErr.Error()))
				return true
			}
			providerErr := classifyAntigravityError(resp.StatusCode, errBody)
			if providerErr.Class == ProviderErrorContext {
				compacted, didCompact, compactErr := compactAntigravityRequestBodyWithRecentTokens(r.URL.Path, body, antigravityEmergencyRecentTokens)
				if compactErr == nil && didCompact {
					antigravityClearNativeReplayOnError(replayScope, resp.StatusCode, errBody)
					logAntigravityUpstreamError(reqID, r.URL.Path, prepared.Format, canonical, resp.StatusCode, errBody, providerErr)
					body = compacted
					contextCompactionRetry = true
					log.Printf("[%s] provider=antigravity context_compaction_retry=true", reqID)
					delete(exclude, account.ID)
					if attempt == 0 {
						attempts++
					} else {
						attempt--
					}
					continue
				}
			}
			resp.Body = io.NopCloser(bytes.NewReader(errBody))
			resp.ContentLength = int64(len(errBody))
		}
		if resp.StatusCode == http.StatusTooManyRequests {
			errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			antigravityClearNativeReplayOnError(replayScope, resp.StatusCode, errBody)
			providerErr := classifyAntigravityError(resp.StatusCode, errBody)
			logAntigravityUpstreamError(reqID, r.URL.Path, prepared.Format, canonical, resp.StatusCode, errBody, providerErr)
			if providerErr.Class == ProviderErrorProtocol || providerErr.Class == ProviderErrorPolicy {
				antigravityWriteError(w, prepared.Format, resp.StatusCode, errBody)
				return true
			}
			if providerErr.Class == ProviderErrorQuota {
				until := providerErr.ResetAt
				if wait, ok := parseRetryAfter(resp.Header); ok && time.Now().Add(wait).After(until) {
					until = time.Now().Add(wait)
				}
				if until.IsZero() {
					until = time.Now().Add(backoffDuration(attempt))
				}
				setAntigravityQuotaCooldown(account, canonical, errBody, until)
				lastRateLimitBody = append(lastRateLimitBody[:0], errBody...)
				lastRateLimitUntil = until
				lastError = fmt.Errorf("Antigravity %s is rate limited until %s", canonical, until.Format(time.RFC3339))
				continue
			}
			lastRateLimitBody = append(lastRateLimitBody[:0], errBody...)
			lastError = fmt.Errorf("Antigravity %s returned %d (%s): %s", canonical, resp.StatusCode, providerErr.Class, safeText(errBody))
			continue
		}
		if resp.StatusCode == http.StatusForbidden {
			errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			antigravityClearNativeReplayOnError(replayScope, resp.StatusCode, errBody)
			logAntigravityUpstreamError(reqID, r.URL.Path, prepared.Format, canonical, resp.StatusCode, errBody, classifyAntigravityError(resp.StatusCode, errBody))
			recordAntigravityForbidden(account, errBody)
			lastError = fmt.Errorf("Antigravity account %s was rejected: %s", account.ID, safeText(errBody))
			continue
		}
		if resp.StatusCode >= 500 {
			// Upstream failures are account-scoped until proven otherwise:
			// rotate instead of failing a request other accounts can serve.
			errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			antigravityClearNativeReplayOnError(replayScope, resp.StatusCode, errBody)
			logAntigravityUpstreamError(reqID, r.URL.Path, prepared.Format, canonical, resp.StatusCode, errBody, classifyAntigravityError(resp.StatusCode, errBody))
			lastFailureStatus, lastFailureBody = resp.StatusCode, errBody
			lastError = fmt.Errorf("Antigravity %s returned %d on account %s", canonical, resp.StatusCode, account.ID)
			// A failure the request itself causes would repeat on every
			// account; bound how many accounts it may consume.
			if serverFailures++; serverFailures >= antigravityMaxServerFailover {
				break
			}
			continue
		}
		if resp.StatusCode < 200 || resp.StatusCode >= 300 {
			errBody, _ := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
			resp.Body.Close()
			antigravityClearNativeReplayOnError(replayScope, resp.StatusCode, errBody)
			logAntigravityUpstreamError(reqID, r.URL.Path, prepared.Format, canonical, resp.StatusCode, errBody, classifyAntigravityError(resp.StatusCode, errBody))
			antigravityWriteError(w, prepared.Format, resp.StatusCode, errBody)
			return true
		}
		clearAntigravityModelCooldown(account, canonical)
		clearAntigravityHealth(account)
		if prepared.Operation == "countTokens" {
			defer resp.Body.Close()
			responseBody, readErr := io.ReadAll(io.LimitReader(resp.Body, 8<<20))
			if readErr != nil {
				antigravityWriteError(w, prepared.Format, http.StatusBadGateway, []byte(readErr.Error()))
				return true
			}
			unwrapped, unwrapErr := unwrapAntigravityResponse(responseBody)
			if unwrapErr != nil {
				antigravityWriteError(w, prepared.Format, http.StatusBadGateway, []byte(unwrapErr.Error()))
				return true
			}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write(unwrapped)
			return true
		}
		if conversationID != "" {
			h.pool.pinForUser(userID, conversationID, account.ID)
		}
		h.writeAntigravityResponse(w, resp, prepared, replayScope, account, conversationID, userID, originID, reqID)
		return true
	}
	if len(lastRateLimitBody) > 0 {
		if !lastRateLimitUntil.IsZero() {
			seconds := int64(time.Until(lastRateLimitUntil).Seconds())
			if seconds < 1 {
				seconds = 1
			}
			w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
		}
		antigravityWriteError(w, antigravityFormatForPath(r.URL.Path), http.StatusTooManyRequests, lastRateLimitBody)
		return true
	}
	if lastFailureStatus != 0 {
		antigravityWriteError(w, antigravityFormatForPath(r.URL.Path), lastFailureStatus, lastFailureBody)
		return true
	}
	if lastError == nil {
		lastError = fmt.Errorf("no Antigravity account currently supports %s", requestedModel)
	}
	antigravityWriteError(w, antigravityFormatForPath(r.URL.Path), http.StatusServiceUnavailable, []byte(lastError.Error()))
	return true
}

// antigravityMaxServerFailover bounds how many accounts a request may try
// after upstream 5xx answers.
const antigravityMaxServerFailover = 3

func recordAntigravityForbidden(account *Account, body []byte) {
	applyAntigravityForbidden(account, body)
	_ = saveAccount(account)
}

func applyAntigravityForbidden(account *Account, body []byte) {
	needsVerification, banned, verificationURL := classifyAntigravityForbidden(body)
	account.mu.Lock()
	defer account.mu.Unlock()
	account.NeedsVerification = needsVerification
	account.VerificationURL = verificationURL
	account.HealthError = strings.TrimSpace(string(body))
	if banned {
		account.Dead = true
	}
	if !needsVerification && !banned {
		account.RateLimitUntil = time.Now().Add(30 * time.Minute)
	}
}

// clearAntigravityHealth drops verification and health markers once the
// daily host has served the account again.
func clearAntigravityHealth(account *Account) {
	account.mu.Lock()
	hadHealthError := account.NeedsVerification || account.VerificationURL != "" || account.HealthError != ""
	account.NeedsVerification, account.VerificationURL, account.HealthError = false, "", ""
	account.mu.Unlock()
	if hadHealthError {
		_ = saveAccount(account)
	}
}

var antigravityVerificationURLPattern = regexp.MustCompile(`https://[^\s"'<>]+`)

func classifyAntigravityForbidden(body []byte) (needsVerification, banned bool, verificationURL string) {
	lower := strings.ToLower(string(body))
	needsVerification = strings.Contains(lower, "validation_required") || strings.Contains(lower, "verify your account") || strings.Contains(lower, "validation_url")
	banned = strings.Contains(lower, "terms of service") || strings.Contains(lower, "terms violation") || strings.Contains(lower, "policy violation") || strings.Contains(lower, "disabled in this account for violation")
	var root any
	if json.Unmarshal(body, &root) == nil {
		var walk func(any) string
		walk = func(value any) string {
			switch node := value.(type) {
			case map[string]any:
				for _, key := range []string{"validation_url", "appeal_url"} {
					if text, _ := node[key].(string); text != "" {
						return text
					}
				}
				for _, child := range node {
					if found := walk(child); found != "" {
						return found
					}
				}
			case []any:
				for _, child := range node {
					if found := walk(child); found != "" {
						return found
					}
				}
			}
			return ""
		}
		verificationURL = walk(root)
		var tosViolation func(any) bool
		tosViolation = func(value any) bool {
			switch node := value.(type) {
			case map[string]any:
				if reason, _ := node["reason"].(string); strings.EqualFold(reason, "TOS_VIOLATION") {
					return true
				}
				for _, child := range node {
					if tosViolation(child) {
						return true
					}
				}
			case []any:
				for _, child := range node {
					if tosViolation(child) {
						return true
					}
				}
			}
			return false
		}
		if tosViolation(root) {
			banned = true
		}
	}
	if verificationURL == "" {
		verificationURL = antigravityVerificationURLPattern.FindString(string(body))
	}
	return needsVerification, banned, verificationURL
}

func antigravityFormatForPath(path string) wire.RequestFormat {
	switch {
	case strings.HasPrefix(path, "/v1/messages"):
		return wire.FormatClaude
	case isAntigravityResponsesPath(path):
		return wire.FormatResponses
	case strings.HasPrefix(path, "/v1/chat/completions"):
		return wire.FormatOpenAI
	default:
		return wire.FormatGemini
	}
}

func logAntigravityUpstreamError(reqID, path string, format wire.RequestFormat, model string, status int, body []byte, providerErr ProviderError) {
	log.Printf("[%s] provider=antigravity path=%s format=%s model=%s status=%d error_class=%s retryable=%t quota_affected=%t summary=%s", reqID, path, format, model, status, providerErr.Class, providerErr.Retryable, providerErr.Class == ProviderErrorQuota, antigravityErrorSummary(body))
}

func isAntigravityResponsesPath(path string) bool {
	path = strings.TrimSuffix(path, "/")
	for _, prefix := range []string{"/responses", "/v1/responses", "/api/codex/responses", "/backend-api/codex/responses"} {
		if path == prefix || strings.HasPrefix(path, prefix+"/") {
			return true
		}
	}
	return false
}

func (h *proxyHandler) doAntigravityRequest(ctx context.Context, incoming http.Header, account *Account, provider *AntigravityProvider, prepared antigravityPreparedRequest) (*http.Response, error) {
	if admission, ok := ctx.Value(policyAdmissionContextKey{}).(*policyAdmission); ok && admission != nil {
		if err := h.checkAccountUse(admission.principalID, account); err != nil {
			return nil, err
		}
	}
	tryBase := func(base *url.URL) (*http.Response, error) {
		u := *base
		operation := prepared.Operation
		if operation == "" {
			operation = "streamGenerateContent"
		}
		u.Path = singleJoin(u.Path, "/v1internal:"+operation)
		if operation == "streamGenerateContent" {
			query := u.Query()
			query.Set("alt", "sse")
			u.RawQuery = query.Encode()
		}
		req, _ := http.NewRequestWithContext(ctx, http.MethodPost, u.String(), bytes.NewReader(prepared.Body))
		provider.SetAuthHeaders(req, account)
		if operation == "streamGenerateContent" {
			req.Header.Set("Accept", "text/event-stream")
		} else {
			req.Header.Set("Accept", "application/json")
		}
		if requestID := incoming.Get("X-Request-ID"); requestID != "" {
			req.Header.Set("X-Request-ID", requestID)
		}
		transport := h.antigravityTransport
		if transport == nil {
			transport = h.transport
		}
		return transport.RoundTrip(req)
	}
	resp, err := tryBase(provider.dailyBase)
	if err == nil && resp.StatusCode == http.StatusTooManyRequests {
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		resp.Body = io.NopCloser(bytes.NewReader(body))
		resp.ContentLength = int64(len(body))
		if classifyAntigravityError(resp.StatusCode, body).Class != ProviderErrorQuota {
			return resp, nil
		}
	}
	if err == nil && resp.StatusCode != http.StatusNotFound && resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode < 500 {
		return resp, nil
	}
	var daily *http.Response
	if resp != nil {
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if readErr == nil {
			resp.Body = io.NopCloser(bytes.NewReader(body))
			resp.ContentLength = int64(len(body))
			daily = resp
		}
	}
	fallback, fallbackErr := tryBase(provider.prodBase)
	if daily == nil || fallbackErr != nil || fallback.StatusCode != http.StatusForbidden {
		return fallback, fallbackErr
	}
	// Production can demand account verification while the daily host keeps
	// serving the same account. The daily answer is the one that describes
	// this account's state, so a production 403 must not replace it.
	_ = fallback.Body.Close()
	return daily, nil
}

func (h *proxyHandler) doAntigravityRequestWithTransientRetry(ctx context.Context, incoming http.Header, account *Account, provider *AntigravityProvider, prepared antigravityPreparedRequest) (*http.Response, error) {
	for attempt := 0; attempt < 2; attempt++ {
		resp, err := h.doAntigravityRequest(ctx, incoming, account, provider, prepared)
		if err != nil || resp == nil {
			return resp, err
		}
		if resp.StatusCode != http.StatusTooManyRequests && resp.StatusCode != http.StatusServiceUnavailable {
			return resp, nil
		}
		body, readErr := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
		_ = resp.Body.Close()
		if readErr != nil {
			return nil, readErr
		}
		delay, retry := antigravityInstantRetryDelay(body, time.Now())
		if antigravityShouldRetryNoCapacity(resp.StatusCode, body) {
			delay, retry = 250*time.Millisecond, true
		}
		if !retry || attempt+1 >= 2 {
			resp.Body = io.NopCloser(bytes.NewReader(body))
			resp.ContentLength = int64(len(body))
			return resp, nil
		}
		timer := time.NewTimer(delay)
		select {
		case <-ctx.Done():
			timer.Stop()
			return nil, ctx.Err()
		case <-timer.C:
		}
	}
	return nil, fmt.Errorf("Antigravity transient retry exhausted")
}

func antigravityInstantRetryDelay(body []byte, now time.Time) (time.Duration, bool) {
	if classifyAntigravityError(http.StatusTooManyRequests, body).Class != ProviderErrorQuota || !strings.Contains(strings.ToLower(string(body)), "rate_limit_exceeded") {
		return 0, false
	}
	until, ok := parseAntigravityRetry(body, now)
	if !ok {
		return 0, false
	}
	delay := until.Sub(now)
	if delay < 0 || delay >= 3*time.Second {
		return 0, false
	}
	if delay > 0 {
		delay += 800 * time.Millisecond
	}
	return delay, true
}

func antigravityShouldRetryNoCapacity(statusCode int, body []byte) bool {
	return statusCode == http.StatusServiceUnavailable && strings.Contains(strings.ToLower(string(body)), "no capacity available")
}

func parseAntigravityRetry(body []byte, now time.Time) (time.Time, bool) {
	if reset, ok := parseGeminiRateLimitReset(body, now); ok {
		return reset, true
	}
	var root any
	if json.Unmarshal(body, &root) != nil {
		return time.Time{}, false
	}
	var walk func(any) string
	walk = func(value any) string {
		switch node := value.(type) {
		case map[string]any:
			for key, child := range node {
				if key == "retryDelay" {
					if text, ok := child.(string); ok {
						return text
					}
				}
				if found := walk(child); found != "" {
					return found
				}
			}
		case []any:
			for _, child := range node {
				if found := walk(child); found != "" {
					return found
				}
			}
		}
		return ""
	}
	if duration, err := time.ParseDuration(walk(root)); err == nil && duration > 0 {
		return now.Add(duration), true
	}
	return time.Time{}, false
}

func (h *proxyHandler) writeAntigravityResponse(w http.ResponseWriter, resp *http.Response, prepared antigravityPreparedRequest, replayScope antigravityReplayScope, account *Account, conversationID, userID, originID, reqID string) {
	defer resp.Body.Close()
	w.Header().Set("X-Accel-Buffering", "no")
	if !prepared.ClientStream {
		result, usage, err := collectAntigravitySSE(resp.Body)
		if err != nil {
			antigravityWriteError(w, prepared.Format, http.StatusBadGateway, []byte(err.Error()))
			return
		}
		wrapped, marshalErr := json.Marshal(map[string]any{"response": result})
		if marshalErr != nil {
			antigravityWriteError(w, prepared.Format, http.StatusBadGateway, []byte(marshalErr.Error()))
			return
		}
		antigravityCaptureNativeReplay(replayScope, prepared.Body, wrapped)
		body, err := translateAntigravityResponseWithRequest(wrapped, prepared.Format, prepared.PublicModel, prepared.ResponsesRequest, prepared.ResponsesFunctionNames, prepared.ResponsesNamespacedTools, prepared.ResponsesCustomTools)
		if err != nil {
			antigravityWriteError(w, prepared.Format, http.StatusBadGateway, []byte(err.Error()))
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write(body)
		if conversationID != "" {
			h.getContextHandoff().MarkNativeSessionEstablished(conversationScopedKey(userID, conversationID), prepared.provider())
			if text := responseTextFromObject(result); text != "" {
				h.getContextHandoff().RecordAssistantText(conversationScopedKey(userID, conversationID), prepared.provider(), text)
			}
		}
		h.recordAntigravityTokenDrift(reqID, prepared, usage)
		h.recordAntigravityUsage(account, usage, prepared.PublicModel, userID, originID, reqID, prepared.provider())
		return
	}
	w.Header().Set("Content-Type", "text/event-stream")
	w.Header().Set("Cache-Control", "no-cache, no-transform")
	w.WriteHeader(http.StatusOK)
	flusher, _ := w.(http.Flusher)
	scanner := bufio.NewScanner(resp.Body)
	scanner.Buffer(make([]byte, 64*1024), 8<<20)
	translator := newAntigravityStreamWriter(w, prepared.Format, prepared.PublicModel)
	translator.setResponsesRequest(prepared.ResponsesRequest)
	translator.setResponsesFunctionNames(prepared.ResponsesFunctionNames)
	translator.setResponsesToolMetadata(prepared.ResponsesNamespacedTools, prepared.ResponsesCustomTools)
	var usage *RequestUsage
	var assistantText strings.Builder
	validResponseSeen := false
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var wrapper map[string]any
		if json.Unmarshal([]byte(data), &wrapper) != nil {
			continue
		}
		response, _ := wrapper["response"].(map[string]any)
		if response == nil {
			response = wrapper
		}
		if _, ok := response["candidates"]; ok {
			validResponseSeen = true
		}
		antigravityCaptureNativeReplay(replayScope, prepared.Body, []byte(data))
		_, _ = translator.Write([]byte("data: " + data + "\n\n"))
		if value := antigravityUsage(response); value != nil {
			usage = value
		}
		assistantText.WriteString(responseTextFromObject(response))
		if flusher != nil {
			flusher.Flush()
		}
	}
	if flusher != nil {
		flusher.Flush()
	}
	if conversationID != "" && validResponseSeen && scanner.Err() == nil {
		h.getContextHandoff().MarkNativeSessionEstablished(conversationScopedKey(userID, conversationID), prepared.provider())
	}
	if conversationID != "" {
		h.getContextHandoff().RecordAssistantText(conversationScopedKey(userID, conversationID), prepared.provider(), assistantText.String())
	}
	h.recordAntigravityTokenDrift(reqID, prepared, usage)
	h.recordAntigravityUsage(account, usage, prepared.PublicModel, userID, originID, reqID, prepared.provider())
}

func (h *proxyHandler) recordAntigravityTokenDrift(reqID string, prepared antigravityPreparedRequest, usage *RequestUsage) {
	if usage == nil || usage.InputTokens <= 0 || prepared.EstimatedInputTokens <= 0 {
		return
	}
	residual := usage.InputTokens - int64(prepared.EstimatedInputTokens)
	if residual > 0 {
		h.metrics.recordAntigravityInputResidual(residual)
	}
	log.Printf("[%s] provider=%s estimated_input_tokens=%d upstream_input_tokens=%d residual=%d", reqID, prepared.provider(), prepared.EstimatedInputTokens, usage.InputTokens, residual)
}

func (h *proxyHandler) recordAntigravityUsage(account *Account, usage *RequestUsage, model, userID, originID, reqID string, provider AccountType) {
	if usage == nil {
		return
	}
	if account != nil {
		usage.AccountID = account.ID
		usage.PlanType = account.PlanType
	}
	usage.Model, usage.UserID, usage.OriginID, usage.RequestID, usage.AccountType = model, userID, originID, reqID, provider
	h.recordUsage(account, *usage)
}

func collectAntigravitySSE(reader io.Reader) (map[string]any, *RequestUsage, error) {
	merged := map[string]any{"candidates": []any{map[string]any{"content": map[string]any{"role": "model", "parts": []any{}}}}}
	parts := []any{}
	var usage *RequestUsage
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 8<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var wrapper map[string]any
		if json.Unmarshal([]byte(data), &wrapper) != nil {
			continue
		}
		response, _ := wrapper["response"].(map[string]any)
		if response == nil {
			response = wrapper
		}
		parts = append(parts, antigravityResponseParts(response)...)
		if value := antigravityUsage(response); value != nil {
			usage = value
			merged["usageMetadata"] = response["usageMetadata"]
		}
		if candidates, _ := response["candidates"].([]any); len(candidates) > 0 {
			candidate, _ := candidates[0].(map[string]any)
			if reason := candidate["finishReason"]; reason != nil {
				mergedCandidates, _ := merged["candidates"].([]any)
				mergedCandidate, _ := mergedCandidates[0].(map[string]any)
				mergedCandidate["finishReason"] = reason
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, usage, err
	}
	candidates, _ := merged["candidates"].([]any)
	candidate, _ := candidates[0].(map[string]any)
	content, _ := candidate["content"].(map[string]any)
	content["parts"] = parts
	return merged, usage, nil
}

func antigravityResponseParts(response map[string]any) []any {
	candidates, _ := response["candidates"].([]any)
	if len(candidates) == 0 {
		return nil
	}
	candidate, _ := candidates[0].(map[string]any)
	content, _ := candidate["content"].(map[string]any)
	parts, _ := content["parts"].([]any)
	return parts
}

func antigravityUsage(response map[string]any) *RequestUsage {
	provider := &AntigravityProvider{}
	return provider.ParseUsage(response)
}

func antigravityWriteError(w http.ResponseWriter, format wire.RequestFormat, status int, body []byte) {
	payload := translateAntigravityError(body, format, status)
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_, _ = w.Write(payload)
}
