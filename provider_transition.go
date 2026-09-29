package main

import (
	"fmt"
	"strings"
)

type TransitionMode string

type TransitionAttempt struct {
	Owner          string
	ConversationID string
	Epoch          uint64
	From           AccountType
	To             AccountType
	RecoveryUsed   bool
}

func (a TransitionAttempt) Key() conversationKey {
	return conversationScopedKey(a.Owner, a.ConversationID)
}

type TransitionCompatibility struct {
	Allowed  bool
	Mode     TransitionMode
	Cost     float64
	Warnings []string
}

func transitionCost(mode TransitionMode) float64 {
	switch mode {
	case TransitionNative:
		return 0
	case TransitionFullHistory:
		return 0.1
	case TransitionSafeHistory:
		return 0.3
	default:
		return 0.7
	}
}

func CanTransition(state ConversationState, from, to AccountType) TransitionCompatibility {
	mode := transitionMode(from, to)
	result := TransitionCompatibility{Allowed: true, Mode: mode, Cost: transitionCost(mode)}
	if mode == TransitionNative {
		return result
	}
	caps := capabilitiesFor(to)
	var hasImage, hasTool, hasParallel bool
	for _, message := range state.IR.Messages {
		calls := 0
		for _, part := range message.Content {
			switch part.Type {
			case IRImage:
				hasImage = true
			case IRToolCall, IRToolResult:
				hasTool = true
				if part.Type == IRToolCall {
					calls++
				}
			}
		}
		if calls > 1 {
			hasParallel = true
		}
	}
	if (hasImage && !caps.SupportsImages) || (hasTool && !caps.SupportsToolHistory) || (hasParallel && !caps.SupportsParallelTools) {
		result.Mode = TransitionSummary
		result.Cost = transitionCost(TransitionSummary)
		result.Warnings = append(result.Warnings, "history_requires_summary")
	}
	if len(state.IR.Messages) > 0 {
		last := state.IR.Messages[len(state.IR.Messages)-1]
		for _, part := range last.Content {
			if part.Type == IRImage && !caps.SupportsImages {
				result.Allowed = false
				result.Warnings = append(result.Warnings, "current_image_unsupported")
			}
			if part.Type == IRToolResult && !caps.SupportsToolHistory {
				result.Allowed = false
				result.Warnings = append(result.Warnings, "current_tool_result_unsupported")
			}
		}
	}
	return result
}

const (
	TransitionNative      TransitionMode = "native"
	TransitionFullHistory TransitionMode = "full-history"
	TransitionSafeHistory TransitionMode = "safe-history"
	TransitionSummary     TransitionMode = "summary"
)

func transitionMode(from, to AccountType) TransitionMode {
	if from == to {
		return TransitionNative
	}
	source, target := capabilitiesFor(from), capabilitiesFor(to)
	if source.SupportsFullHistoryHandoff && target.SupportsFullHistoryHandoff &&
		source.WireFormat == target.WireFormat {
		return TransitionFullHistory
	}
	if target.SupportsForeignHistory {
		return TransitionSafeHistory
	}
	return TransitionSummary
}

func translateFallbackPayload(body []byte, path string, target AccountType) ([]byte, string, TranslateDirection, error) {
	if target == AccountTypeAntigravity {
		return body, path, TranslateNone, nil
	}
	if strings.HasPrefix(path, "/v1/responses") || strings.HasPrefix(path, "/responses") {
		switch capabilitiesFor(target).WireFormat {
		case WireResponses:
			return body, "/v1/responses", TranslateNone, nil
		case WireAnthropic:
			translated, err := translateResponsesToClaudeRequest(body)
			return translated, "/v1/messages", TranslateResponsesToClaude, err
		}
	}
	if strings.HasPrefix(path, "/v1/messages") {
		switch capabilitiesFor(target).WireFormat {
		case WireAnthropic:
			return body, path, TranslateNone, nil
		case WireResponses:
			translated, err := translateClaudeToResponsesRequest(body)
			return translated, "/v1/responses", TranslateClaudeToResponses, err
		case WireOpenAI:
			translated, err := translateRequestBody(body, FormatClaude, FormatOpenAI)
			return translated, "/v1/chat/completions", TranslateClaudeToOAI, err
		}
	}
	if strings.HasPrefix(path, "/v1/chat/completions") {
		switch capabilitiesFor(target).WireFormat {
		case WireOpenAI:
			return body, path, TranslateNone, nil
		case WireResponses:
			translated, err := translateChatCompletionsToResponses(body)
			return translated, "/v1/responses", TranslateChatToResponses, err
		case WireAnthropic:
			translated, err := translateRequestBody(body, FormatOpenAI, FormatClaude)
			return translated, "/v1/messages", TranslateOAIToClaude, err
		}
	}
	return nil, "", TranslateNone, fmt.Errorf("unsupported fallback translation from %s to %s", path, target)
}

// summaryHandoffMessages uses only visible text and the current request.
// Provider-native objects, tool IDs, and opaque reasoning stay out of this
// universal fallback path.
func summaryHandoffMessages(previous, current []Message, priorSummary string) []Message {
	var summary strings.Builder
	if priorSummary != "" {
		summary.WriteString(priorSummary)
		summary.WriteString("\n")
	}
	for _, message := range previous {
		for _, part := range message.Parts {
			if part.Type != "text" || part.Text == "" {
				continue
			}
			line := message.Role + ": " + part.Text + "\n"
			if summary.Len()+len(line) > contextHandoffSummaryChars {
				break
			}
			summary.WriteString(line)
		}
		if summary.Len() >= contextHandoffSummaryChars {
			break
		}
	}
	output := []Message{{Role: "system", Parts: []MessagePart{{Type: "text", Text: "Earlier conversation summary:\n" + summary.String()}}}}
	// A small visible tail helps preserve immediate conversational context.
	start := len(previous) - 4
	if start < 0 {
		start = 0
	}
	for _, message := range previous[start:] {
		copyMessage := Message{Role: message.Role}
		for _, part := range message.Parts {
			if part.Type == "text" {
				copyMessage.Parts = append(copyMessage.Parts, part)
			}
		}
		if len(copyMessage.Parts) > 0 {
			output = append(output, copyMessage)
		}
	}
	for _, message := range current {
		copyMessage := Message{Role: message.Role}
		for _, part := range message.Parts {
			if part.Type == "text" {
				copyMessage.Parts = append(copyMessage.Parts, part)
			}
		}
		if len(copyMessage.Parts) > 0 {
			output = append(output, copyMessage)
		}
	}
	return output
}
