package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"log"
	"net/http"
	"strings"
	"sync"
	"time"
)

const (
	contextHandoffMaxConversations = 2048
	contextHandoffCompactTokens    = 100_000
	contextHandoffRecentTokens     = 48_000
	contextHandoffSummaryChars     = 16_000
)

// ConversationState is the provider-independent state retained by the pool.
// Provider-local identifiers are deliberately stored outside Messages.
type ConversationState struct {
	ID               string                                `json:"id"`
	Messages         []Message                             `json:"messages"`
	Tools            []ToolCall                            `json:"tools,omitempty"`
	Summary          string                                `json:"summary,omitempty"`
	Metadata         map[string]any                        `json:"metadata,omitempty"`
	ProviderState    map[string]ProviderLocalState         `json:"provider_state,omitempty"`
	ProviderSessions map[AccountType]*ProviderSessionState `json:"provider_sessions,omitempty"`
	ActiveProvider   AccountType                           `json:"active_provider,omitempty"`
	TransitionEpoch  uint64                                `json:"transition_epoch,omitempty"`
	UpdatedAt        time.Time                             `json:"updated_at"`
}

type Message struct {
	Role  string        `json:"role"`
	Parts []MessagePart `json:"parts"`
}

type MessagePart struct {
	Type      string `json:"type"`
	Text      string `json:"text,omitempty"`
	ToolID    string `json:"tool_id,omitempty"`
	ToolName  string `json:"tool_name,omitempty"`
	Arguments string `json:"arguments,omitempty"`
}

type ToolCall struct {
	ID        string `json:"id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments,omitempty"`
	Result    string `json:"result,omitempty"`
}

type ProviderLocalState struct {
	Identifiers map[string]any `json:"identifiers,omitempty"`
}

// ProviderSessionState owns identifiers that have meaning only to one
// upstream. The conversation ID remains stable across provider changes.
type ProviderSessionState struct {
	Provider        AccountType    `json:"provider"`
	NativeSessionID string         `json:"native_session_id,omitempty"`
	ResponseID      string         `json:"response_id,omitempty"`
	CacheKey        string         `json:"cache_key,omitempty"`
	Epoch           uint64         `json:"epoch"`
	Metadata        map[string]any `json:"metadata,omitempty"`
}

type contextHandoffResult struct {
	Switched  bool
	From      AccountType
	Mode      TransitionMode
	Compacted bool
	Warnings  []string
}

type conversationHandoffRecord struct {
	State        ConversationState
	LastProvider AccountType
}

type conversationHandoffStore struct {
	mu      sync.Mutex
	records map[string]conversationHandoffRecord
}

func newConversationHandoffStore() *conversationHandoffStore {
	return &conversationHandoffStore{records: make(map[string]conversationHandoffRecord)}
}

type contextWireFormat int

const (
	contextFormatUnknown contextWireFormat = iota
	contextFormatResponses
	contextFormatOpenAI
	contextFormatClaude
	contextFormatGemini
)

func contextRequestObject(root map[string]any) map[string]any {
	if response, ok := root["response"].(map[string]any); ok {
		if eventType, _ := root["type"].(string); eventType == "response.create" {
			return response
		}
	}
	if request, ok := root["request"].(map[string]any); ok {
		if _, hasContents := request["contents"]; hasContents {
			return request
		}
	}
	return root
}

func detectContextWireFormat(path string, object map[string]any) contextWireFormat {
	if _, ok := object["input"]; ok || strings.Contains(path, "/responses") {
		return contextFormatResponses
	}
	if _, ok := object["contents"]; ok {
		return contextFormatGemini
	}
	if _, ok := object["messages"]; ok {
		if strings.HasPrefix(path, "/v1/messages") {
			return contextFormatClaude
		}
		if _, ok := object["system"]; ok {
			return contextFormatClaude
		}
		return contextFormatOpenAI
	}
	return contextFormatUnknown
}

func appendTextPart(parts []MessagePart, text string) []MessagePart {
	text = strings.TrimSpace(text)
	if text == "" {
		return parts
	}
	return append(parts, MessagePart{Type: "text", Text: text})
}

func contextText(value any) string {
	switch typed := value.(type) {
	case string:
		return typed
	case []any:
		var parts []string
		for _, item := range typed {
			switch block := item.(type) {
			case string:
				if strings.TrimSpace(block) != "" {
					parts = append(parts, block)
				}
			case map[string]any:
				if text, _ := block["text"].(string); strings.TrimSpace(text) != "" {
					parts = append(parts, text)
				} else if text, _ := block["content"].(string); strings.TrimSpace(text) != "" {
					parts = append(parts, text)
				}
			}
		}
		return strings.Join(parts, "\n")
	default:
		if typed == nil {
			return ""
		}
		encoded, _ := json.Marshal(typed)
		return string(encoded)
	}
}

func contextArguments(value any) string {
	if text, ok := value.(string); ok {
		return text
	}
	encoded, _ := json.Marshal(value)
	if len(encoded) == 0 || string(encoded) == "null" {
		return "{}"
	}
	return string(encoded)
}

func normalizeResponsesContext(object map[string]any) []Message {
	var messages []Message
	if instructions := contextText(object["instructions"]); instructions != "" {
		messages = append(messages, Message{Role: "system", Parts: []MessagePart{{Type: "text", Text: instructions}}})
	}
	switch input := object["input"].(type) {
	case string:
		messages = append(messages, Message{Role: "user", Parts: []MessagePart{{Type: "text", Text: input}}})
	case []any:
		for _, raw := range input {
			item, ok := raw.(map[string]any)
			if !ok {
				continue
			}
			itemType, _ := item["type"].(string)
			switch itemType {
			case "message", "":
				role, _ := item["role"].(string)
				parts := appendTextPart(nil, contextText(item["content"]))
				if role != "" && len(parts) > 0 {
					messages = append(messages, Message{Role: role, Parts: parts})
				}
			case "function_call":
				messages = append(messages, Message{Role: "assistant", Parts: []MessagePart{{
					Type: "tool_call", ToolID: stringValue(item["call_id"]),
					ToolName: stringValue(item["name"]), Arguments: contextArguments(item["arguments"]),
				}}})
			case "function_call_output":
				messages = append(messages, Message{Role: "tool", Parts: []MessagePart{{
					Type: "tool_result", ToolID: stringValue(item["call_id"]), Text: contextText(item["output"]),
				}}})
			}
		}
	}
	return messages
}

func normalizeMessageList(object map[string]any, claude bool) []Message {
	var messages []Message
	if claude {
		if system := contextText(object["system"]); system != "" {
			messages = append(messages, Message{Role: "system", Parts: []MessagePart{{Type: "text", Text: system}}})
		}
	}
	rawMessages, _ := object["messages"].([]any)
	for _, raw := range rawMessages {
		item, ok := raw.(map[string]any)
		if !ok {
			continue
		}
		role, _ := item["role"].(string)
		message := Message{Role: role}
		switch content := item["content"].(type) {
		case string:
			message.Parts = appendTextPart(message.Parts, content)
		case []any:
			for _, rawPart := range content {
				part, ok := rawPart.(map[string]any)
				if !ok {
					continue
				}
				partType, _ := part["type"].(string)
				switch partType {
				case "text", "input_text", "output_text":
					message.Parts = appendTextPart(message.Parts, stringValue(part["text"]))
				case "tool_use":
					message.Parts = append(message.Parts, MessagePart{
						Type: "tool_call", ToolID: stringValue(part["id"]),
						ToolName: stringValue(part["name"]), Arguments: contextArguments(part["input"]),
					})
				case "tool_result":
					message.Parts = append(message.Parts, MessagePart{
						Type: "tool_result", ToolID: stringValue(part["tool_use_id"]), Text: contextText(part["content"]),
					})
				}
			}
		}
		if calls, ok := item["tool_calls"].([]any); ok {
			for _, rawCall := range calls {
				call, _ := rawCall.(map[string]any)
				function, _ := call["function"].(map[string]any)
				message.Parts = append(message.Parts, MessagePart{
					Type: "tool_call", ToolID: stringValue(call["id"]),
					ToolName: stringValue(function["name"]), Arguments: contextArguments(function["arguments"]),
				})
			}
		}
		if role == "tool" {
			message.Parts = append(message.Parts, MessagePart{
				Type: "tool_result", ToolID: stringValue(item["tool_call_id"]), Text: contextText(item["content"]),
			})
		}
		if role != "" && len(message.Parts) > 0 {
			messages = append(messages, message)
		}
	}
	return messages
}

func normalizeGeminiContext(object map[string]any) []Message {
	var messages []Message
	if instruction, ok := object["systemInstruction"].(map[string]any); ok {
		if text := contextText(instruction["parts"]); text != "" {
			messages = append(messages, Message{Role: "system", Parts: []MessagePart{{Type: "text", Text: text}}})
		}
	}
	contents, _ := object["contents"].([]any)
	for _, raw := range contents {
		content, _ := raw.(map[string]any)
		role := stringValue(content["role"])
		if role == "model" {
			role = "assistant"
		}
		message := Message{Role: role}
		parts, _ := content["parts"].([]any)
		for _, rawPart := range parts {
			part, _ := rawPart.(map[string]any)
			if text := stringValue(part["text"]); text != "" {
				message.Parts = appendTextPart(message.Parts, text)
			}
			if call, ok := part["functionCall"].(map[string]any); ok {
				message.Parts = append(message.Parts, MessagePart{
					Type: "tool_call", ToolID: stringValue(call["id"]),
					ToolName: stringValue(call["name"]), Arguments: contextArguments(call["args"]),
				})
			}
			if response, ok := part["functionResponse"].(map[string]any); ok {
				message.Parts = append(message.Parts, MessagePart{
					Type: "tool_result", ToolID: stringValue(response["id"]),
					ToolName: stringValue(response["name"]), Text: contextText(response["response"]),
				})
			}
		}
		if role != "" && len(message.Parts) > 0 {
			messages = append(messages, message)
		}
	}
	return messages
}

func normalizeConversationMessages(format contextWireFormat, object map[string]any) []Message {
	switch format {
	case contextFormatResponses:
		return normalizeResponsesContext(object)
	case contextFormatOpenAI:
		return normalizeMessageList(object, false)
	case contextFormatClaude:
		return normalizeMessageList(object, true)
	case contextFormatGemini:
		return normalizeGeminiContext(object)
	default:
		return nil
	}
}

func messageFingerprint(message Message) string {
	encoded, _ := json.Marshal(message)
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:])
}

func mergeConversationMessages(previous, current []Message) []Message {
	if len(previous) == 0 {
		return append([]Message(nil), current...)
	}
	if len(current) == 0 {
		return append([]Message(nil), previous...)
	}
	maxOverlap := len(previous)
	if len(current) < maxOverlap {
		maxOverlap = len(current)
	}
	overlap := 0
	for size := maxOverlap; size > 0; size-- {
		matched := true
		for index := 0; index < size; index++ {
			if messageFingerprint(previous[len(previous)-size+index]) != messageFingerprint(current[index]) {
				matched = false
				break
			}
		}
		if matched {
			overlap = size
			break
		}
	}
	merged := append([]Message(nil), previous...)
	return append(merged, current[overlap:]...)
}

func precedingToolTriggerIndex(messages []Message, callIndex int) int {
	for index := callIndex - 1; index >= 0; index-- {
		if len(messages[index].Parts) == 0 {
			continue
		}
		switch messages[index].Role {
		case "system", "developer":
			continue
		case "user", "tool":
			return index
		default:
			return -1
		}
	}
	return -1
}

func sanitizeConversationToolPairs(messages []Message) ([]Message, []string) {
	type toolLocation struct {
		callIndex   int
		resultIndex int
	}
	locations := make(map[string]toolLocation)
	for messageIndex, message := range messages {
		for _, part := range message.Parts {
			if part.ToolID == "" {
				continue
			}
			location := locations[part.ToolID]
			switch part.Type {
			case "tool_call":
				if location.callIndex == 0 && messageIndex != 0 {
					location.callIndex = messageIndex + 1
				} else if messageIndex == 0 {
					location.callIndex = 1
				}
			case "tool_result":
				if location.resultIndex == 0 && messageIndex != 0 {
					location.resultIndex = messageIndex + 1
				} else if messageIndex == 0 {
					location.resultIndex = 1
				}
			}
			locations[part.ToolID] = location
		}
	}

	valid := make(map[string]bool)
	var warnings []string
	for id, location := range locations {
		callIndex := location.callIndex - 1
		resultIndex := location.resultIndex - 1
		if location.callIndex == 0 {
			warnings = appendUniqueString(warnings, "orphan_tool_result_dropped")
			continue
		}
		if location.resultIndex == 0 {
			warnings = appendUniqueString(warnings, "dangling_tool_call_dropped")
			continue
		}
		if resultIndex < callIndex {
			warnings = appendUniqueString(warnings, "invalid_tool_pair_order_dropped")
			continue
		}
		if precedingToolTriggerIndex(messages, callIndex) < 0 {
			warnings = appendUniqueString(warnings, "invalid_tool_call_prefix_dropped")
			continue
		}
		valid[id] = true
	}

	out := make([]Message, 0, len(messages))
	for _, message := range messages {
		copyMessage := Message{Role: message.Role}
		for _, part := range message.Parts {
			switch part.Type {
			case "tool_call":
				if !valid[part.ToolID] {
					continue
				}
			case "tool_result":
				if !valid[part.ToolID] {
					continue
				}
			}
			copyMessage.Parts = append(copyMessage.Parts, part)
		}
		if len(copyMessage.Parts) > 0 {
			out = append(out, copyMessage)
		}
	}
	return out, warnings
}

type conversationMessageRange struct {
	start int
	end   int
}

func conversationAtomicRanges(messages []Message) []conversationMessageRange {
	type pair struct {
		callIndex   int
		resultIndex int
	}
	pairs := make(map[string]pair)
	for messageIndex, message := range messages {
		for _, part := range message.Parts {
			if part.ToolID == "" {
				continue
			}
			current := pairs[part.ToolID]
			switch part.Type {
			case "tool_call":
				current.callIndex = messageIndex + 1
			case "tool_result":
				current.resultIndex = messageIndex + 1
			}
			pairs[part.ToolID] = current
		}
	}

	var linked []conversationMessageRange
	for _, current := range pairs {
		if current.callIndex == 0 || current.resultIndex == 0 {
			continue
		}
		callIndex := current.callIndex - 1
		resultIndex := current.resultIndex - 1
		if resultIndex < callIndex {
			continue
		}
		start := callIndex
		if trigger := precedingToolTriggerIndex(messages, callIndex); trigger >= 0 {
			start = trigger
		}
		linked = append(linked, conversationMessageRange{start: start, end: resultIndex})
	}

	var ranges []conversationMessageRange
	for index := 0; index < len(messages); {
		end := index
		for {
			expanded := false
			for _, candidate := range linked {
				if candidate.start <= end && candidate.end >= index && candidate.end > end {
					end = candidate.end
					expanded = true
				}
			}
			if !expanded {
				break
			}
		}
		ranges = append(ranges, conversationMessageRange{start: index, end: end})
		index = end + 1
	}
	return ranges
}

func messageCharacterSize(message Message) int {
	size := len(message.Role) + 8
	for _, part := range message.Parts {
		size += len(part.Text) + len(part.ToolName) + len(part.Arguments) + 16
	}
	return size
}

func rangeCharacterSize(messages []Message, value conversationMessageRange) int {
	size := 0
	for index := value.start; index <= value.end; index++ {
		size += messageCharacterSize(messages[index])
	}
	return size
}

func messageHasToolParts(message Message) bool {
	for _, part := range message.Parts {
		if part.Type == "tool_call" || part.Type == "tool_result" {
			return true
		}
	}
	return false
}

func trimTextOnlyMessage(message Message, characterBudget int) (Message, bool) {
	if characterBudget <= len(message.Role)+8 || messageHasToolParts(message) {
		return Message{}, false
	}
	remaining := characterBudget - len(message.Role) - 8
	reversed := make([]MessagePart, 0, len(message.Parts))
	for index := len(message.Parts) - 1; index >= 0 && remaining > 16; index-- {
		part := message.Parts[index]
		if part.Type != "text" {
			continue
		}
		size := len(part.Text) + 16
		if size <= remaining {
			reversed = append(reversed, part)
			remaining -= size
			continue
		}
		keep := remaining - 16
		if keep > len(part.Text) {
			keep = len(part.Text)
		}
		if keep > 0 {
			part.Text = part.Text[len(part.Text)-keep:]
			reversed = append(reversed, part)
		}
		remaining = 0
	}
	if len(reversed) == 0 {
		return Message{}, false
	}
	parts := make([]MessagePart, len(reversed))
	for index := range reversed {
		parts[len(reversed)-1-index] = reversed[index]
	}
	return Message{Role: message.Role, Parts: parts}, true
}

func regenerateToolCallIDs(messages []Message, conversationID string, provider AccountType) ([]Message, []string) {
	idMap := make(map[string]string)
	callIndex := 0
	for messageIndex := range messages {
		for partIndex := range messages[messageIndex].Parts {
			part := &messages[messageIndex].Parts[partIndex]
			if part.Type != "tool_call" {
				continue
			}
			seed := fmt.Sprintf("%s|%s|%d|%s|%s", conversationID, provider, callIndex, part.ToolName, part.Arguments)
			sum := sha256.Sum256([]byte(seed))
			newID := "call_" + hex.EncodeToString(sum[:8])
			if part.ToolID != "" {
				idMap[part.ToolID] = newID
			}
			part.ToolID = newID
			callIndex++
		}
	}
	var warnings []string
	for messageIndex := range messages {
		filtered := messages[messageIndex].Parts[:0]
		for _, part := range messages[messageIndex].Parts {
			if part.Type == "tool_result" {
				if replacement := idMap[part.ToolID]; replacement != "" {
					part.ToolID = replacement
				} else {
					warnings = appendUniqueString(warnings, "orphan_tool_result_dropped")
					continue
				}
			}
			filtered = append(filtered, part)
		}
		messages[messageIndex].Parts = filtered
	}
	return messages, warnings
}

func toolsFromMessages(messages []Message) []ToolCall {
	calls := make(map[string]*ToolCall)
	var order []string
	for _, message := range messages {
		for _, part := range message.Parts {
			switch part.Type {
			case "tool_call":
				call := &ToolCall{ID: part.ToolID, Name: part.ToolName, Arguments: part.Arguments}
				calls[part.ToolID] = call
				order = append(order, part.ToolID)
			case "tool_result":
				if call := calls[part.ToolID]; call != nil {
					call.Result = part.Text
				}
			}
		}
	}
	out := make([]ToolCall, 0, len(order))
	for _, id := range order {
		if call := calls[id]; call != nil {
			out = append(out, *call)
		}
	}
	return out
}

func estimateContextTokens(messages []Message) int {
	characters := 0
	for _, message := range messages {
		characters += len(message.Role) + 8
		for _, part := range message.Parts {
			characters += len(part.Text) + len(part.ToolName) + len(part.Arguments) + 16
		}
	}
	return (characters + 3) / 4
}

func compactConversationMessages(messages []Message) ([]Message, string, bool) {
	if estimateContextTokens(messages) <= contextHandoffCompactTokens {
		return messages, "", false
	}

	recent, recentStart := trimConversationMessagesWithStart(messages, contextHandoffRecentTokens*4)
	if len(recent) == 0 {
		return messages, "", false
	}

	var summary strings.Builder
	summary.WriteString("Earlier conversation compacted by codex-pool:\n")
	for _, message := range messages[:recentStart] {
		for _, part := range message.Parts {
			if part.Type != "text" || part.Text == "" {
				continue
			}
			line := message.Role + ": " + part.Text + "\n"
			remaining := contextHandoffSummaryChars - summary.Len()
			if remaining <= 0 {
				break
			}
			if len(line) > remaining {
				line = line[:remaining]
			}
			summary.WriteString(line)
		}
		if summary.Len() >= contextHandoffSummaryChars {
			break
		}
	}
	summaryText := strings.TrimSpace(summary.String())
	compacted := []Message{{
		Role:  "system",
		Parts: []MessagePart{{Type: "text", Text: summaryText}},
	}}
	compacted = append(compacted, recent...)
	return compacted, summaryText, true
}

func trimConversationMessagesWithStart(messages []Message, characterBudget int) ([]Message, int) {
	if characterBudget <= 0 || len(messages) == 0 {
		return nil, len(messages)
	}
	ranges := conversationAtomicRanges(messages)
	remaining := characterBudget
	selectedRange := len(ranges)
	var leadingTrimmed *Message

	for rangeIndex := len(ranges) - 1; rangeIndex >= 0; rangeIndex-- {
		current := ranges[rangeIndex]
		size := rangeCharacterSize(messages, current)
		if size <= remaining {
			remaining -= size
			selectedRange = rangeIndex
			continue
		}

		if current.start == current.end && !messageHasToolParts(messages[current.start]) {
			if trimmed, ok := trimTextOnlyMessage(messages[current.start], remaining); ok {
				leadingTrimmed = &trimmed
				selectedRange = rangeIndex
			}
		}
		break
	}

	if selectedRange == len(ranges) {
		// Correctness wins over the target budget: retain the latest atomic
		// tool exchange whole rather than creating a dangling call/result.
		last := ranges[len(ranges)-1]
		out := make([]Message, last.end-last.start+1)
		copy(out, messages[last.start:last.end+1])
		for index := range out {
			out[index].Parts = append([]MessagePart(nil), out[index].Parts...)
		}
		return out, last.start
	}

	start := ranges[selectedRange].start
	var out []Message
	if leadingTrimmed != nil {
		out = append(out, *leadingTrimmed)
		start = ranges[selectedRange].start
		selectedRange++
	}
	for rangeIndex := selectedRange; rangeIndex < len(ranges); rangeIndex++ {
		current := ranges[rangeIndex]
		for messageIndex := current.start; messageIndex <= current.end; messageIndex++ {
			copyMessage := messages[messageIndex]
			copyMessage.Parts = append([]MessagePart(nil), copyMessage.Parts...)
			out = append(out, copyMessage)
		}
	}
	return out, start
}

func trimConversationMessages(messages []Message, characterBudget int) []Message {
	out, _ := trimConversationMessagesWithStart(messages, characterBudget)
	return out
}

func appendUniqueString(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func extractProviderLocalState(object map[string]any) ProviderLocalState {
	keys := []string{
		"previous_response_id", "prompt_cache_key", "prompt_cache_id",
		"session_id", "sessionId", "response_id",
	}
	state := ProviderLocalState{Identifiers: make(map[string]any)}
	for _, key := range keys {
		if value, ok := object[key]; ok {
			state.Identifiers[key] = value
		}
	}
	if len(state.Identifiers) == 0 {
		state.Identifiers = nil
	}
	return state
}

func recordProviderSession(state *ConversationState, provider AccountType, local ProviderLocalState) {
	if provider == "" {
		return
	}
	if state.ProviderSessions == nil {
		state.ProviderSessions = make(map[AccountType]*ProviderSessionState)
	}
	session := state.ProviderSessions[provider]
	if session == nil {
		session = &ProviderSessionState{Provider: provider, Epoch: state.TransitionEpoch}
		state.ProviderSessions[provider] = session
	}
	if len(local.Identifiers) == 0 {
		return
	}
	if session.Metadata == nil {
		session.Metadata = make(map[string]any)
	}
	for key, value := range local.Identifiers {
		session.Metadata[key] = value
		switch key {
		case "session_id", "sessionId":
			session.NativeSessionID = stringValue(value)
		case "previous_response_id", "response_id":
			session.ResponseID = stringValue(value)
		case "prompt_cache_key", "prompt_cache_id":
			session.CacheKey = stringValue(value)
		}
	}
}

func stripIncompatibleProviderFields(value any) bool {
	changed := false
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			switch strings.ToLower(key) {
			case "previous_response_id", "prompt_cache_key", "prompt_cache_id",
				"session_id", "sessionid", "response_id",
				"thought_signature", "thoughtsignature", "encrypted_content",
				"reasoning_metadata":
				delete(typed, key)
				changed = true
				continue
			}
			if stripIncompatibleProviderFields(child) {
				changed = true
			}
		}
	case []any:
		for _, child := range typed {
			if stripIncompatibleProviderFields(child) {
				changed = true
			}
		}
	}
	return changed
}

func detectContextWarnings(object map[string]any, switched bool) []string {
	if !switched {
		return nil
	}
	var warnings []string
	for _, key := range []string{"previous_response_id", "prompt_cache_key", "prompt_cache_id"} {
		if _, ok := object[key]; ok {
			warnings = appendUniqueString(warnings, "opaque_provider_state_dropped")
		}
	}
	if tools, ok := object["tools"].([]any); ok {
		for _, raw := range tools {
			tool, _ := raw.(map[string]any)
			toolType := stringValue(tool["type"])
			if toolType != "" && toolType != "function" {
				warnings = appendUniqueString(warnings, "provider_specific_tool_requires_support")
			}
		}
	}
	return warnings
}

func renderResponsesContext(object map[string]any, messages []Message) {
	var input []any
	var instructions []string
	for _, message := range messages {
		if message.Role == "system" || message.Role == "developer" {
			for _, part := range message.Parts {
				if part.Type == "text" && part.Text != "" {
					instructions = append(instructions, part.Text)
				}
			}
			continue
		}
		var content []any
		var actions []any
		for _, part := range message.Parts {
			switch part.Type {
			case "text":
				contentType := "input_text"
				if message.Role == "assistant" {
					contentType = "output_text"
				}
				content = append(content, map[string]any{"type": contentType, "text": part.Text})
			case "tool_call":
				actions = append(actions, map[string]any{
					"type": "function_call", "call_id": part.ToolID,
					"name": part.ToolName, "arguments": part.Arguments,
				})
			case "tool_result":
				actions = append(actions, map[string]any{
					"type": "function_call_output", "call_id": part.ToolID, "output": part.Text,
				})
			}
		}
		if len(content) > 0 {
			input = append(input, map[string]any{"type": "message", "role": message.Role, "content": content})
		}
		input = append(input, actions...)
	}
	if len(instructions) > 0 {
		object["instructions"] = strings.Join(instructions, "\n\n")
	}
	object["input"] = input
}

func renderOpenAIContext(object map[string]any, messages []Message) {
	var output []any
	for _, message := range messages {
		if message.Role == "tool" {
			for _, part := range message.Parts {
				if part.Type == "tool_result" {
					output = append(output, map[string]any{
						"role": "tool", "tool_call_id": part.ToolID, "content": part.Text,
					})
				}
			}
			continue
		}
		item := map[string]any{"role": message.Role}
		var text []string
		var calls []any
		for _, part := range message.Parts {
			switch part.Type {
			case "text":
				text = append(text, part.Text)
			case "tool_call":
				calls = append(calls, map[string]any{
					"id": part.ToolID, "type": "function",
					"function": map[string]any{"name": part.ToolName, "arguments": part.Arguments},
				})
			}
		}
		item["content"] = strings.Join(text, "\n")
		if len(calls) > 0 {
			item["tool_calls"] = calls
		}
		if item["content"] != "" || len(calls) > 0 {
			output = append(output, item)
		}
	}
	object["messages"] = output
}

func renderClaudeContext(object map[string]any, messages []Message) {
	var system []string
	var output []map[string]any
	for _, message := range messages {
		if message.Role == "system" || message.Role == "developer" {
			for _, part := range message.Parts {
				if part.Type == "text" {
					system = append(system, part.Text)
				}
			}
			continue
		}
		role := message.Role
		if role == "tool" {
			role = "user"
		}
		var blocks []any
		for _, part := range message.Parts {
			switch part.Type {
			case "text":
				blocks = append(blocks, map[string]any{"type": "text", "text": part.Text})
			case "tool_call":
				var arguments any
				if json.Unmarshal([]byte(part.Arguments), &arguments) != nil {
					arguments = map[string]any{}
				}
				blocks = append(blocks, map[string]any{
					"type": "tool_use", "id": part.ToolID, "name": part.ToolName, "input": arguments,
				})
			case "tool_result":
				blocks = append(blocks, map[string]any{
					"type": "tool_result", "tool_use_id": part.ToolID, "content": part.Text,
				})
			}
		}
		if len(blocks) > 0 {
			output = append(output, map[string]any{"role": role, "content": blocks})
		}
	}
	if len(system) > 0 {
		object["system"] = strings.Join(system, "\n\n")
	}
	output = mergeConsecutiveClaudeMessages(output)
	output = normalizeClaudeToolPairing(output)
	output = mergeConsecutiveClaudeMessages(output)
	wire := make([]any, len(output))
	for index, message := range output {
		wire[index] = message
	}
	object["messages"] = wire
}

func renderGeminiContext(object map[string]any, messages []Message) {
	var system []any
	var contents []any
	for _, message := range messages {
		if message.Role == "system" || message.Role == "developer" {
			for _, part := range message.Parts {
				if part.Type == "text" {
					system = append(system, map[string]any{"text": part.Text})
				}
			}
			continue
		}
		role := message.Role
		if role == "assistant" {
			role = "model"
		}
		if role == "tool" {
			role = "user"
		}
		var parts []any
		for _, part := range message.Parts {
			switch part.Type {
			case "text":
				parts = append(parts, map[string]any{"text": part.Text})
			case "tool_call":
				var arguments any
				if json.Unmarshal([]byte(part.Arguments), &arguments) != nil {
					arguments = map[string]any{}
				}
				parts = append(parts, map[string]any{"functionCall": map[string]any{
					"id": part.ToolID, "name": part.ToolName, "args": arguments,
				}})
			case "tool_result":
				parts = append(parts, map[string]any{"functionResponse": map[string]any{
					"id": part.ToolID, "name": part.ToolName, "response": map[string]any{"result": part.Text},
				}})
			}
		}
		if len(parts) > 0 {
			contents = append(contents, map[string]any{"role": role, "parts": parts})
		}
	}
	if len(system) > 0 {
		object["systemInstruction"] = map[string]any{"parts": system}
	}
	object["contents"] = contents
}

func renderConversationMessages(format contextWireFormat, object map[string]any, messages []Message) {
	switch format {
	case contextFormatResponses:
		renderResponsesContext(object, messages)
	case contextFormatOpenAI:
		renderOpenAIContext(object, messages)
	case contextFormatClaude:
		renderClaudeContext(object, messages)
	case contextFormatGemini:
		renderGeminiContext(object, messages)
	}
}

func (s *conversationHandoffStore) evictOldestLocked() {
	if len(s.records) < contextHandoffMaxConversations {
		return
	}
	oldestID := ""
	var oldest time.Time
	for id, record := range s.records {
		if oldestID == "" || record.State.UpdatedAt.Before(oldest) {
			oldestID = id
			oldest = record.State.UpdatedAt
		}
	}
	delete(s.records, oldestID)
}

func (s *conversationHandoffStore) Prepare(conversationID string, target AccountType, path string, body []byte) ([]byte, contextHandoffResult, error) {
	if s == nil || conversationID == "" || target == "" || len(body) == 0 {
		return body, contextHandoffResult{}, nil
	}
	var root map[string]any
	if err := json.Unmarshal(body, &root); err != nil {
		return body, contextHandoffResult{}, nil
	}
	object := contextRequestObject(root)
	format := detectContextWireFormat(path, object)
	if format == contextFormatUnknown {
		return body, contextHandoffResult{}, nil
	}
	current := normalizeConversationMessages(format, object)
	localState := extractProviderLocalState(object)

	s.mu.Lock()
	defer s.mu.Unlock()
	record, exists := s.records[conversationID]
	switched := exists && record.LastProvider != "" && record.LastProvider != target
	mode := TransitionNative
	if switched {
		mode = transitionMode(record.LastProvider, target)
	}
	result := contextHandoffResult{Switched: switched, From: record.LastProvider, Mode: mode, Warnings: detectContextWarnings(object, switched)}

	messages := current
	previouslyCompacted := exists && record.State.Summary != ""
	if switched {
		if mode == TransitionSummary {
			messages = summaryHandoffMessages(record.State.Messages, current, record.State.Summary)
		} else {
			messages = mergeConversationMessages(record.State.Messages, current)
		}
		stripIncompatibleProviderFields(root)
		if len(record.State.Messages) == 0 {
			result.Warnings = appendUniqueString(result.Warnings, "normalized_history_unavailable")
		}
	}

	var warnings []string
	messages, warnings = sanitizeConversationToolPairs(messages)
	for _, warning := range warnings {
		result.Warnings = appendUniqueString(result.Warnings, warning)
	}

	var summary string
	var compacted bool
	messages, summary, compacted = compactConversationMessages(messages)

	// Compaction can change the cut boundary. Validate tool pairing again after
	// the cut, then regenerate provider-local IDs only for surviving pairs.
	messages, warnings = sanitizeConversationToolPairs(messages)
	for _, warning := range warnings {
		result.Warnings = appendUniqueString(result.Warnings, warning)
	}
	if switched {
		messages, warnings = regenerateToolCallIDs(messages, conversationID, target)
		for _, warning := range warnings {
			result.Warnings = appendUniqueString(result.Warnings, warning)
		}
		renderConversationMessages(format, object, messages)
		result.Compacted = compacted || previouslyCompacted
	}

	if !exists {
		s.evictOldestLocked()
		record.State = ConversationState{
			ID: conversationID, Metadata: make(map[string]any),
			ProviderState:    make(map[string]ProviderLocalState),
			ProviderSessions: make(map[AccountType]*ProviderSessionState),
		}
	}
	if record.State.ProviderState == nil {
		record.State.ProviderState = make(map[string]ProviderLocalState)
	}
	if record.State.ProviderSessions == nil {
		record.State.ProviderSessions = make(map[AccountType]*ProviderSessionState)
	}
	if switched {
		// The incoming request still belongs to the client conversation. Any
		// opaque identifier it carries belongs to the previous upstream.
		if len(localState.Identifiers) > 0 {
			record.State.ProviderState[string(record.LastProvider)] = localState
			recordProviderSession(&record.State, record.LastProvider, localState)
		}
		record.State.TransitionEpoch++
		// Re-entry begins a new native epoch; old provider state is retained
		// under its own provider but must not be supplied to this request.
		record.State.ProviderSessions[target] = &ProviderSessionState{Provider: target, Epoch: record.State.TransitionEpoch}
		record.State.ProviderState[string(target)] = ProviderLocalState{}
	} else {
		if len(localState.Identifiers) > 0 {
			record.State.ProviderState[string(target)] = localState
		}
		recordProviderSession(&record.State, target, localState)
	}
	record.State.Messages = messages
	record.State.Tools = toolsFromMessages(messages)
	if summary != "" {
		record.State.Summary = summary
	}
	record.State.ActiveProvider = target
	record.State.UpdatedAt = time.Now().UTC()
	record.LastProvider = target
	s.records[conversationID] = record

	// Same-provider requests are never rewritten by the handoff store. Native
	// providers already own their context/compaction semantics; the compacted
	// representation above is only the pool's internal copy for a future switch.
	if !switched {
		return body, result, nil
	}
	rewritten, err := json.Marshal(root)
	if err != nil {
		return nil, result, err
	}
	return rewritten, result, nil
}

func (s *conversationHandoffStore) State(conversationID string) (ConversationState, bool) {
	if s == nil {
		return ConversationState{}, false
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[conversationID]
	return record.State, ok
}

func (s *conversationHandoffStore) RecordAssistantText(conversationID string, provider AccountType, text string) {
	text = strings.TrimSpace(text)
	if s == nil || conversationID == "" || provider == "" || text == "" {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	record, ok := s.records[conversationID]
	if !ok {
		s.evictOldestLocked()
		record.State = ConversationState{
			ID: conversationID, Metadata: make(map[string]any),
			ProviderState:    make(map[string]ProviderLocalState),
			ProviderSessions: make(map[AccountType]*ProviderSessionState),
		}
	}
	message := Message{Role: "assistant", Parts: []MessagePart{{Type: "text", Text: text}}}
	if count := len(record.State.Messages); count > 0 &&
		messageFingerprint(record.State.Messages[count-1]) == messageFingerprint(message) {
		recordProviderSession(&record.State, provider, ProviderLocalState{})
		record.State.UpdatedAt = time.Now().UTC()
		record.State.ActiveProvider = provider
		record.LastProvider = provider
		s.records[conversationID] = record
		return
	}
	record.State.Messages = append(record.State.Messages, message)
	record.State.Messages, _ = sanitizeConversationToolPairs(record.State.Messages)
	record.State.Messages, record.State.Summary, _ = compactConversationMessages(record.State.Messages)
	record.State.Messages, _ = sanitizeConversationToolPairs(record.State.Messages)
	record.State.Tools = toolsFromMessages(record.State.Messages)
	recordProviderSession(&record.State, provider, ProviderLocalState{})
	record.State.UpdatedAt = time.Now().UTC()
	record.State.ActiveProvider = provider
	record.LastProvider = provider
	s.records[conversationID] = record
}

func responseTextFromObject(object map[string]any) string {
	var parts []string
	if delta, _ := object["delta"].(string); delta != "" {
		parts = append(parts, delta)
	}
	if delta, ok := object["delta"].(map[string]any); ok {
		if text, _ := delta["text"].(string); text != "" {
			parts = append(parts, text)
		}
	}
	if response, ok := object["response"].(map[string]any); ok {
		if text := responseTextFromObject(response); text != "" {
			parts = append(parts, text)
		}
	}
	if output, ok := object["output"].([]any); ok {
		for _, raw := range output {
			item, _ := raw.(map[string]any)
			if content, ok := item["content"].([]any); ok {
				for _, rawContent := range content {
					block, _ := rawContent.(map[string]any)
					if text, _ := block["text"].(string); text != "" {
						parts = append(parts, text)
					}
				}
			}
		}
	}
	if content, ok := object["content"].([]any); ok {
		for _, raw := range content {
			block, _ := raw.(map[string]any)
			if text, _ := block["text"].(string); text != "" {
				parts = append(parts, text)
			}
		}
	}
	if choices, ok := object["choices"].([]any); ok {
		for _, raw := range choices {
			choice, _ := raw.(map[string]any)
			for _, key := range []string{"delta", "message"} {
				container, _ := choice[key].(map[string]any)
				if text, _ := container["content"].(string); text != "" {
					parts = append(parts, text)
				}
			}
		}
	}
	if candidates, ok := object["candidates"].([]any); ok {
		for _, raw := range candidates {
			candidate, _ := raw.(map[string]any)
			content, _ := candidate["content"].(map[string]any)
			rawParts, _ := content["parts"].([]any)
			for _, rawPart := range rawParts {
				part, _ := rawPart.(map[string]any)
				if thought, _ := part["thought"].(bool); thought {
					continue
				}
				if text, _ := part["text"].(string); text != "" {
					parts = append(parts, text)
				}
			}
		}
	}
	return strings.Join(parts, "")
}

func extractAssistantTextFromResponseSample(sample []byte) string {
	sample = bytes.TrimSpace(sample)
	if len(sample) == 0 {
		return ""
	}
	var direct map[string]any
	if json.Unmarshal(sample, &direct) == nil {
		return strings.TrimSpace(responseTextFromObject(direct))
	}
	var deltas strings.Builder
	var completed string
	scanner := bufio.NewScanner(bytes.NewReader(sample))
	scanner.Buffer(make([]byte, 64*1024), 2<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(data), &event) != nil {
			continue
		}
		text := responseTextFromObject(event)
		if text == "" {
			continue
		}
		eventType := stringValue(event["type"])
		if strings.Contains(eventType, "delta") {
			deltas.WriteString(text)
		} else {
			completed = text
		}
	}
	if deltas.Len() > 0 {
		return strings.TrimSpace(deltas.String())
	}
	return strings.TrimSpace(completed)
}

func (h *proxyHandler) getContextHandoff() *conversationHandoffStore {
	if h == nil {
		return nil
	}
	h.contextHandoffOnce.Do(func() {
		h.contextHandoff = newConversationHandoffStore()
	})
	return h.contextHandoff
}

func (h *proxyHandler) prepareProviderContextHandoff(
	w http.ResponseWriter,
	conversationID string,
	target AccountType,
	path string,
	body []byte,
) ([]byte, error) {
	store := h.getContextHandoff()
	if store == nil {
		return body, nil
	}
	rewritten, result, err := store.Prepare(conversationID, target, path, body)
	if err != nil {
		return nil, err
	}
	if result.Switched {
		w.Header().Set("X-Pool-Context-Handoff", "provider-switch")
		w.Header().Set("X-Pool-Transition-Mode", string(result.Mode))
		log.Printf("conversation=%s transition=%s->%s mode=%s", conversationID, result.From, target, result.Mode)
	}
	if result.Compacted {
		w.Header().Set("X-Pool-Context-Compacted", "true")
	}
	if len(result.Warnings) > 0 {
		w.Header().Set("X-Pool-Context-Warning", strings.Join(result.Warnings, ","))
	}
	return rewritten, nil
}
