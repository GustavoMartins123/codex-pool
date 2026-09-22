package main

import "strings"

type TransitionMode string

type TransitionAttempt struct {
	ConversationID string
	Epoch          uint64
	From           AccountType
	To             AccountType
	RecoveryUsed   bool
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
