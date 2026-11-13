package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"
)

func (h *proxyHandler) serveAIStudioAdmin(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/admin/aistudio")
	switch {
	case (path == "" || path == "/") && r.Method == http.MethodGet:
		h.handleAPIKeyList(w, r, AccountTypeAIStudio)
	case path == "/add" && r.Method == http.MethodPost:
		h.handleAIStudioAdd(w, r)
	case strings.HasSuffix(path, "/remove") && r.Method == http.MethodPost:
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/"), "/remove")
		h.handleAPIKeyRemove(w, r, AccountTypeAIStudio, id)
	default:
		http.NotFound(w, r)
	}
}

// handleAIStudioAdd stores a Google AI Studio API key after the Gemini API
// lists its models, so a key that cannot route is never added.
func (h *proxyHandler) handleAIStudioAdd(w http.ResponseWriter, r *http.Request) {
	if !h.checkContributionAttempt(w, r) {
		return
	}
	var input struct {
		APIKey string `json:"api_key"`
	}
	if err := decodeGovernanceJSON(w, r, &input); err != nil {
		respondJSONError(w, http.StatusBadRequest, "invalid AI Studio account request")
		return
	}
	key := strings.TrimSpace(input.APIKey)
	if key == "" {
		respondJSONError(w, http.StatusBadRequest, "api_key is required")
		return
	}
	provider := h.registry.ForType(AccountTypeAIStudio)
	if provider == nil {
		respondJSONError(w, http.StatusServiceUnavailable, "AI Studio provider is unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	snapshot, err := fetchProviderModels(ctx, h.transport, provider, &Account{Type: AccountTypeAIStudio, AccessToken: key})
	if err != nil {
		var upstream *providerModelHTTPError
		// The Gemini API answers an unknown key with 400 API_KEY_INVALID.
		if errors.As(err, &upstream) && (upstream.StatusCode == http.StatusBadRequest || upstream.StatusCode == http.StatusUnauthorized || upstream.StatusCode == http.StatusForbidden) {
			respondJSONError(w, http.StatusBadRequest, "invalid AI Studio API key or Gemini API access denied")
		} else {
			respondJSONError(w, http.StatusBadGateway, "AI Studio model discovery failed; the key was not added")
		}
		return
	}
	id, err := h.saveContribution(r, AccountTypeAIStudio, key, map[string]any{
		"api_key": key, "added_at": time.Now().UTC().Format(time.RFC3339Nano),
		"provider_model_snapshot": snapshot,
	})
	if err != nil {
		respondPolicyError(w, err)
		return
	}
	respondJSON(w, map[string]any{"success": true, "account_id": id})
}
