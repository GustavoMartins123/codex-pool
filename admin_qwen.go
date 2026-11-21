package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"time"

	"go.etcd.io/bbolt"
)

func (h *proxyHandler) serveQwenAdmin(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/admin/qwen")
	switch {
	case (path == "" || path == "/") && r.Method == http.MethodGet:
		h.handleAPIKeyList(w, r, AccountTypeQwen)
	case path == "/add" && r.Method == http.MethodPost:
		h.handleQwenAdd(w, r)
	case strings.HasSuffix(path, "/remove") && r.Method == http.MethodPost:
		id := strings.TrimSuffix(strings.TrimPrefix(path, "/"), "/remove")
		h.handleAPIKeyRemove(w, r, AccountTypeQwen, id)
	default:
		http.NotFound(w, r)
	}
}

func (h *proxyHandler) handleQwenAdd(w http.ResponseWriter, r *http.Request) {
	if !h.checkContributionAttempt(w, r) {
		return
	}
	var input struct {
		APIKey    string               `json:"api_key"`
		TokenPlan *QwenTokenPlanConfig `json:"token_plan,omitempty"`
	}
	if err := decodeGovernanceJSON(w, r, &input); err != nil {
		respondJSONError(w, http.StatusBadRequest, "invalid Qwen account request")
		return
	}
	key := strings.TrimSpace(input.APIKey)
	if key == "" {
		respondJSONError(w, http.StatusBadRequest, "api_key is required")
		return
	}
	if err := validateQwenTokenPlan(input.TokenPlan, time.Now()); err != nil {
		respondJSONError(w, http.StatusBadRequest, err.Error())
		return
	}
	if input.TokenPlan != nil && input.TokenPlan.Mode == qwenPersonalManual {
		input.TokenPlan.UpdatedAt = time.Now().UTC()
	}
	provider := h.registry.ForType(AccountTypeQwen)
	if provider == nil {
		respondJSONError(w, http.StatusServiceUnavailable, "Qwen provider is unavailable")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
	defer cancel()
	account := &Account{Type: AccountTypeQwen, AccessToken: key}
	snapshot, err := fetchProviderModels(ctx, h.transport, provider, account)
	if err != nil {
		var upstream *providerModelHTTPError
		if errors.As(err, &upstream) && (upstream.StatusCode == http.StatusUnauthorized || upstream.StatusCode == http.StatusForbidden) {
			respondJSONError(w, http.StatusBadRequest, "invalid Qwen API key or Token Plan access denied")
		} else {
			respondJSONError(w, http.StatusBadGateway, "Qwen model discovery failed; the account was not added")
		}
		return
	}
	credits, err := h.queryQwenCredits(ctx, input.TokenPlan, time.Now())
	if err != nil {
		respondJSONError(w, http.StatusBadGateway, err.Error())
		return
	}
	id, err := h.saveContribution(r, AccountTypeQwen, key, map[string]any{
		"api_key": key, "added_at": time.Now().UTC().Format(time.RFC3339Nano),
		"provider_model_snapshot": snapshot, "token_plan": input.TokenPlan, "token_plan_credits": credits,
	})
	if err != nil {
		respondPolicyError(w, err)
		return
	}
	respondJSON(w, map[string]any{"success": true, "account_id": id})
}

func (h *proxyHandler) handleQwenTokenPlan(w http.ResponseWriter, r *http.Request, id string) {
	noStore(w)
	if h.passport == nil || h.pool == nil {
		respondJSONError(w, http.StatusServiceUnavailable, "account authority unavailable")
		return
	}
	actor, session := h.passport.authenticate(r)
	if actor == nil {
		respondJSONError(w, http.StatusUnauthorized, "authentication required")
		return
	}
	if err := h.passport.authorizeAccount(actor.ID, &Account{Type: AccountTypeQwen, ID: id}, "manage"); err != nil {
		respondJSONError(w, http.StatusForbidden, "account management denied")
		return
	}
	var account *Account
	for _, candidate := range h.pool.allAccounts() {
		if candidate.Type == AccountTypeQwen && candidate.ID == id {
			account = candidate
			break
		}
	}
	if account == nil {
		respondJSONError(w, http.StatusNotFound, "Qwen account not found")
		return
	}
	if r.Method != http.MethodGet && r.Method != http.MethodPut && r.Method != http.MethodPost {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return
	}
	if r.Method != http.MethodGet {
		if !h.passportCSRF(r, session) {
			respondJSONError(w, http.StatusForbidden, "invalid CSRF token")
			return
		}
		account.mu.Lock()
		plan := account.qwen.TokenPlan
		account.mu.Unlock()
		if r.Method == http.MethodPut {
			var input struct {
				TokenPlan *QwenTokenPlanConfig `json:"token_plan"`
			}
			if decodeGovernanceJSON(w, r, &input) != nil {
				respondJSONError(w, http.StatusBadRequest, "invalid Token Plan configuration")
				return
			}
			plan = input.TokenPlan
			if err := validateQwenTokenPlan(plan, time.Now()); err != nil {
				respondJSONError(w, http.StatusBadRequest, err.Error())
				return
			}
			if plan != nil && plan.Mode == qwenPersonalManual {
				plan.UpdatedAt = time.Now().UTC()
			}
		}
		ctx, cancel := context.WithTimeout(r.Context(), 20*time.Second)
		defer cancel()
		credits, err := h.queryQwenCredits(ctx, plan, time.Now())
		if err != nil {
			if r.Method == http.MethodPost {
				account.mu.Lock()
				account.qwen.Credits = nil
				account.qwen.CreditsError = err.Error()
				account.Usage = UsageSnapshot{Source: "qwen-token-plan", RetrievedAt: time.Now(), primarySet: true, secondarySet: true, creditsSet: true}
				persistErr := saveAccount(h.pool.antigravity, account)
				account.mu.Unlock()
				if persistErr != nil {
					respondJSONError(w, http.StatusInternalServerError, "credit refresh failed and unavailable state could not be saved")
					return
				}
			}
			respondJSONError(w, http.StatusBadGateway, err.Error())
			return
		}
		if err := h.passport.authorizeAccount(actor.ID, account, "manage"); err != nil {
			respondJSONError(w, http.StatusForbidden, "account management denied")
			return
		}
		account.mu.Lock()
		if r.Method == http.MethodPost && account.qwen.TokenPlan != plan {
			account.mu.Unlock()
			respondJSONError(w, http.StatusConflict, "Token Plan configuration changed during credit refresh")
			return
		}
		previousPlan, previousCredits, previousError, previousUsage, previousType := account.qwen.TokenPlan, account.qwen.Credits, account.qwen.CreditsError, account.Usage, account.PlanType
		account.qwen.TokenPlan, account.qwen.Credits, account.qwen.CreditsError = plan, credits, ""
		account.PlanType = "token_plan"
		account.Usage = UsageSnapshot{Source: "qwen-token-plan", RetrievedAt: time.Now(), primarySet: true, secondarySet: true, creditsSet: true}
		if credits != nil {
			account.PlanType = "token_plan_" + credits.Edition
			account.Usage = qwenCreditUsage(credits, time.Now())
		} else {
			account.qwen.CreditsError = "Token Plan credit tracking is not configured"
		}
		if err := saveAccount(h.pool.antigravity, account); err != nil {
			account.qwen.TokenPlan, account.qwen.Credits, account.qwen.CreditsError, account.Usage, account.PlanType = previousPlan, previousCredits, previousError, previousUsage, previousType
			account.mu.Unlock()
			respondJSONError(w, http.StatusInternalServerError, "could not save Token Plan configuration")
			return
		}
		account.mu.Unlock()
		if err := h.passport.db.Update(func(tx *bbolt.Tx) error {
			return h.passport.audit(tx, actor.ID, "account.token_plan_updated", id, "qwen")
		}); err != nil {
			respondJSONError(w, http.StatusInternalServerError, "Token Plan configuration saved; audit recording failed")
			return
		}
	}
	account.mu.Lock()
	defer account.mu.Unlock()
	var safePlan *QwenTokenPlanConfig
	if account.qwen.TokenPlan != nil {
		copy := *account.qwen.TokenPlan
		copy.AccessKeyID, copy.AccessKeySecret, copy.SecurityToken = "", "", ""
		copy.CloudOAuth = nil
		safePlan = &copy
	}
	credits, creditsError := account.qwen.Credits, account.qwen.CreditsError
	if !qwenCreditsCurrent(credits, time.Now()) {
		credits = nil
		if creditsError == "" {
			creditsError = "Token Plan credit data is unavailable or expired; refresh the balance"
		}
	}
	respondJSON(w, map[string]any{"token_plan": safePlan, "credits": credits, "error": creditsError})
}
