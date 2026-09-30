package main

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"net/http"
	"strings"
	"time"
)

// serveKimiAdmin routes Kimi admin requests (auth already checked by router)
func (h *proxyHandler) serveKimiAdmin(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/admin/kimi")
	if path == "" {
		path = "/"
	}

	switch {
	case path == "/" && r.Method == http.MethodGet:
		h.handleAPIKeyList(w, r, AccountTypeKimi)

	case path == "/add" && r.Method == http.MethodPost:
		h.handleKimiAdd(w, r)

	case strings.HasSuffix(path, "/remove") && r.Method == http.MethodPost:
		id := strings.TrimPrefix(path, "/")
		id = strings.TrimSuffix(id, "/remove")
		h.handleAPIKeyRemove(w, r, AccountTypeKimi, id)

	default:
		http.NotFound(w, r)
	}
}

// POST /admin/kimi/add - add a Kimi API key
func (h *proxyHandler) handleKimiAdd(w http.ResponseWriter, r *http.Request) {
	if !h.checkContributionAttempt(w,r) { return }
	var req struct {
		APIKey string `json:"api_key"`
	}

	if r.Header.Get("Content-Type") == "application/json" {
		if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
			respondJSONError(w, http.StatusBadRequest, "invalid json: "+err.Error())
			return
		}
	} else {
		req.APIKey = r.FormValue("api_key")
	}

	apiKey := strings.TrimSpace(req.APIKey)
	if apiKey == "" {
		respondJSONError(w, http.StatusBadRequest, "api_key is required")
		return
	}

	// Validate key by calling GET /v1/models
	validationURL := h.cfg.kimiBase.String() + "/v1/models"
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	validReq, _ := http.NewRequestWithContext(ctx, http.MethodGet, validationURL, nil)
	validReq.Header.Set("Authorization", "Bearer "+apiKey)

	resp, err := h.transport.RoundTrip(validReq)
	if err != nil {
		respondJSONError(w, http.StatusBadGateway, "failed to validate key: "+err.Error())
		return
	}
	_, _ = io.Copy(io.Discard, io.LimitReader(resp.Body, 1<<20))
	resp.Body.Close()

	if resp.StatusCode == http.StatusUnauthorized || resp.StatusCode == http.StatusForbidden {
		respondJSONError(w, http.StatusBadRequest, "invalid API key (authentication failed)")
		return
	}
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		respondJSONError(w, http.StatusBadGateway, fmt.Sprintf("key validation returned status %d", resp.StatusCode))
		return
	}

	h.saveAPIKeyAccountFile(w, r, AccountTypeKimi, "kimi", apiKey)
}

// handleAPIKeyList lists all accounts of the given type.
func (h *proxyHandler) handleAPIKeyList(w http.ResponseWriter, r *http.Request, acctType AccountType) {
	accounts := h.requestVisiblePool(r).allAccounts()

	type accountInfo struct {
		ID       string        `json:"id"`
		PlanType string        `json:"plan_type"`
		Dead     bool          `json:"dead"`
		Disabled bool          `json:"disabled"`
		Usage    UsageSnapshot `json:"usage,omitempty"`
	}

	var result []accountInfo
	for _, acc := range accounts {
		if acc.Type != acctType {
			continue
		}
		acc.mu.Lock()
		info := accountInfo{
			ID:       acc.ID,
			PlanType: acc.PlanType,
			Dead:     acc.Dead,
			Disabled: acc.Disabled,
			Usage:    acc.Usage,
		}
		acc.mu.Unlock()
		result = append(result, info)
	}

	respondJSON(w, map[string]any{
		"accounts": result,
		"count":    len(result),
	})
}

// handleAPIKeyRemove marks an account as dead.
func (h *proxyHandler) handleAPIKeyRemove(w http.ResponseWriter, r *http.Request, acctType AccountType, accountID string) {
	if !h.authorizeAccountManagement(w, r, accountID) { return }
	accounts := h.requestVisiblePool(r).allAccounts()
	var target *Account
	for _, acc := range accounts {
		if acc.Type == acctType && acc.ID == accountID {
			target = acc
			break
		}
	}

	if target == nil {
		respondJSONError(w, http.StatusNotFound, "account not found")
		return
	}

	target.mu.Lock()
	target.Dead = true
	target.Penalty += 100.0
	target.LastPenalty = time.Now()
	target.mu.Unlock()

	if err := saveAccount(target); err != nil {
		log.Printf("warning: failed to save dead %s account %s: %v", acctType, accountID, err)
		respondJSONError(w, http.StatusInternalServerError, "failed to persist: "+err.Error())
		return
	}

	log.Printf("removed %s account %s (marked dead)", acctType, accountID)

	respondJSON(w, map[string]any{
		"success":    true,
		"account_id": accountID,
	})
}

// saveAPIKeyAccountFile creates a new API key account file and reloads accounts.
func (h *proxyHandler) saveAPIKeyAccountFile(w http.ResponseWriter, r *http.Request, acctType AccountType, subdir, apiKey string) {
	accountID, err := h.saveContribution(r, acctType, apiKey, map[string]any{"api_key": apiKey, "added_at": time.Now().UTC().Format(time.RFC3339Nano)})
	if err != nil {
		respondPolicyError(w, err)
		return
	}

	respondJSON(w, map[string]any{
		"success":    true,
		"account_id": accountID,
	})
}
