package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"strings"
)

type transitionDryRunRequest struct {
	ConversationID string          `json:"conversation_id"`
	From           AccountType     `json:"from"`
	To             AccountType     `json:"to"`
	Model          string          `json:"model"`
	Path           string          `json:"path,omitempty"`
	Request        json.RawMessage `json:"request,omitempty"`
}

type transitionDryRunResponse struct {
	Mode           TransitionMode `json:"mode"`
	FreshSession   bool           `json:"fresh_session"`
	RemovedFields  []string       `json:"removed_fields"`
	MessagesBefore int            `json:"messages_before"`
	MessagesAfter  int            `json:"messages_after"`
	ToolPairs      int            `json:"tool_pairs"`
	ToolPairsValid int            `json:"tool_pairs_valid"`
	Warnings       []string       `json:"warnings"`
}

func transitionDryRunWire(provider AccountType) (contextWireFormat, string) {
	switch capabilitiesFor(provider).WireFormat {
	case WireResponses:
		return contextFormatResponses, "/v1/responses"
	case WireAnthropic:
		return contextFormatClaude, "/v1/messages"
	case WireGemini:
		return contextFormatGemini, "/v1beta/models/gemini:generateContent"
	case WireOpenAI:
		return contextFormatOpenAI, "/v1/chat/completions"
	default:
		return contextFormatUnknown, ""
	}
}

func countTransitionToolPairs(messages []Message) (int, int) {
	calls := make(map[string]int)
	results := make(map[string]int)
	for _, message := range messages {
		for _, part := range message.Parts {
			switch part.Type {
			case "tool_call":
				calls[part.ToolID]++
			case "tool_result":
				results[part.ToolID]++
			}
		}
	}
	valid := 0
	for id, count := range calls {
		if id != "" && count == 1 && results[id] == 1 {
			valid++
		}
	}
	return len(calls), valid
}

func (h *proxyHandler) handleTransitionDryRun(w http.ResponseWriter, r *http.Request) {
	if !h.checkAdminAuth(w, r) {
		return
	}
	if r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	var input transitionDryRunRequest
	if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, 4<<20)).Decode(&input); err != nil {
		respondJSONError(w, http.StatusBadRequest, "invalid transition request")
		return
	}
	if strings.TrimSpace(input.ConversationID) == "" || input.From == "" || input.To == "" {
		respondJSONError(w, http.StatusBadRequest, "conversation_id, from and to are required")
		return
	}
	format, defaultPath := transitionDryRunWire(input.To)
	if format == contextFormatUnknown {
		respondJSONError(w, http.StatusBadRequest, "unsupported target provider")
		return
	}
	store := h.getContextHandoff()
	store.mu.Lock()
	record, ok := store.records[input.ConversationID]
	if !ok || record.LastProvider != input.From {
		store.mu.Unlock()
		respondJSONError(w, http.StatusNotFound, "conversation or source provider not found")
		return
	}
	// JSON cloning isolates nested maps, slices and native state. Prepare runs
	// only against this temporary store and cannot change the live conversation.
	copyBytes, err := json.Marshal(record)
	store.mu.Unlock()
	if err != nil {
		respondJSONError(w, http.StatusInternalServerError, "cannot clone conversation")
		return
	}
	var copied conversationHandoffRecord
	if json.Unmarshal(copyBytes, &copied) != nil {
		respondJSONError(w, http.StatusInternalServerError, "cannot clone conversation")
		return
	}
	clone := newConversationHandoffStore()
	clone.records[input.ConversationID] = copied
	path := input.Path
	if path == "" {
		path = defaultPath
	}
	body := []byte(input.Request)
	if len(body) == 0 {
		object := map[string]any{"model": input.Model}
		renderConversationIR(format, object, conversationIRFromMessages(copied.State.Messages, input.ConversationID, copied.State.TransitionEpoch))
		body, err = json.Marshal(object)
		if err != nil {
			respondJSONError(w, http.StatusInternalServerError, "cannot render conversation")
			return
		}
	}
	_, result, err := clone.Prepare(input.ConversationID, input.To, path, body)
	if err != nil {
		respondJSONError(w, http.StatusUnprocessableEntity, err.Error())
		return
	}
	after, _ := clone.State(input.ConversationID)
	pairs, valid := countTransitionToolPairs(after.Messages)
	removed := append([]string(nil), result.RemovedState...)
	if input.From != input.To {
		for name := range copied.State.ProviderState[string(input.From)].Identifiers {
			removed = appendUniqueString(removed, name)
		}
	}
	if removed == nil {
		removed = []string{}
	}
	sort.Strings(removed)
	warnings := result.Warnings
	if warnings == nil {
		warnings = []string{}
	}
	respondJSON(w, transitionDryRunResponse{
		Mode: result.Mode, FreshSession: result.Switched, RemovedFields: removed,
		MessagesBefore: len(copied.State.Messages), MessagesAfter: len(after.Messages),
		ToolPairs: pairs, ToolPairsValid: valid, Warnings: warnings,
	})
}
