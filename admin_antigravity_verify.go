package main

import (
	"context"
	"errors"
	"log"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// antigravityVerifyResult is the on-demand access check answer. The probe is
// the zero-cost quota summary endpoint, never a generation request.
type antigravityVerifyResult struct {
	AccountID       string `json:"account_id"`
	Status          string `json:"status"`
	Detail          string `json:"detail,omitempty"`
	VerificationURL string `json:"verification_url,omitempty"`
	AppealURL       string `json:"appeal_url,omitempty"`
	CheckedAt       string `json:"checked_at"`
}

// Google serves appeal and verification links from these hosts. The links
// carry continuation tokens, so anything off-list is dropped before it can
// reach an operator's browser.
var antigravityAppealHosts = map[string]struct{}{
	"accounts.google.com": {},
	"forms.gle":           {},
	"docs.google.com":     {},
}

func safeAntigravityAppealURL(raw string) string {
	u, err := url.Parse(strings.TrimSpace(raw))
	if err != nil || u.Scheme != "https" || u.User != nil {
		return ""
	}
	if _, ok := antigravityAppealHosts[u.Hostname()]; !ok {
		return ""
	}
	if port := u.Port(); port != "" && port != "443" {
		return ""
	}
	return u.String()
}

func antigravityVerifyDetail(text string) string {
	text = safeText([]byte(text))
	if len(text) > 512 {
		text = text[:512]
	}
	return text
}

func antigravityQuotaExhausted(summary AntigravityQuotaSummary) bool {
	enabled := 0
	hasQuota := func(bucket AntigravityQuotaBucket) bool {
		if bucket.Disabled {
			return false
		}
		enabled++
		return (bucket.RemainingFraction != nil && *bucket.RemainingFraction > 0) ||
			(bucket.RemainingAmount != nil && *bucket.RemainingAmount > 0)
	}
	for _, bucket := range summary.Buckets {
		if hasQuota(bucket) {
			return false
		}
	}
	for _, group := range summary.Groups {
		for _, bucket := range group.Buckets {
			if hasQuota(bucket) {
				return false
			}
		}
	}
	return enabled > 0
}

func (h *proxyHandler) verifyAntigravityAccount(ctx context.Context, acc *Account, daily, production *url.URL) antigravityVerifyResult {
	result := antigravityVerifyResult{AccountID: acc.ID, CheckedAt: time.Now().UTC().Format(time.RFC3339)}
	fail := func(status, detail string) antigravityVerifyResult {
		result.Status = status
		result.Detail = antigravityVerifyDetail(detail)
		return result
	}

	acc.mu.Lock()
	expired := acc.ExpiresAt.IsZero() || acc.ExpiresAt.Before(time.Now().Add(5*time.Minute))
	hasRefreshToken := acc.RefreshToken != ""
	acc.mu.Unlock()
	if expired {
		if !hasRefreshToken {
			return fail("error", "access token expired and no refresh token is stored; relogin required")
		}
		if err := h.refreshAccount(ctx, acc); err != nil {
			if isPermanentRefreshTokenError(err) {
				persistDeadAccount(acc, "refresh token revoked/invalid")
				return fail("revoked", err.Error())
			}
			return fail("error", err.Error())
		}
	}

	summary, err := fetchAntigravityQuotaSummary(ctx, h.transport, acc, daily, production)
	var forbidden *antigravityForbiddenError
	if errors.As(err, &forbidden) {
		recordAntigravityForbidden(acc, forbidden.body)
		needsVerification, banned, verificationURL := classifyAntigravityForbidden(forbidden.body)
		detail := antigravityErrorSummary(forbidden.body)
		switch {
		case banned:
			result.AppealURL = safeAntigravityAppealURL(verificationURL)
			return fail("banned", detail)
		case needsVerification:
			result.VerificationURL = safeAntigravityAppealURL(verificationURL)
			return fail("needs_verification", detail)
		default:
			return fail("error", detail)
		}
	}
	if err != nil {
		return fail("error", err.Error())
	}

	clearAntigravityHealth(acc)
	if antigravityQuotaExhausted(summary) {
		return fail("quota_exhausted", "all enabled quota buckets are exhausted")
	}
	result.Status = "healthy"
	return result
}

func (h *proxyHandler) auditAntigravityVerify(r *http.Request, result antigravityVerifyResult) {
	if h.passport == nil {
		return
	}
	if err := h.passport.recordAudit(providerContributionActor(r), "account.verify", result.AccountID, result.Status); err != nil {
		log.Printf("warning: failed to audit access verification for account %s: %v", result.AccountID, err)
	}
}

func (h *proxyHandler) handleAccountVerify(w http.ResponseWriter, r *http.Request, accountID string) {
	var target *Account
	for _, a := range h.pool.allAccounts() {
		if a.ID == accountID {
			target = a
			break
		}
	}
	if target == nil {
		respondJSONError(w, http.StatusNotFound, "account not found")
		return
	}
	if target.Type != AccountTypeAntigravity {
		respondJSONError(w, http.StatusBadRequest, "access verification is only available for Antigravity accounts")
		return
	}
	provider, _ := h.registry.ForType(AccountTypeAntigravity).(*AntigravityProvider)
	if provider == nil {
		respondJSONError(w, http.StatusServiceUnavailable, "Antigravity provider is not configured")
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	result := h.verifyAntigravityAccount(ctx, target, provider.DailyURL(), provider.ProductionURL())
	h.auditAntigravityVerify(r, result)
	respondJSON(w, result)
}

func (h *proxyHandler) handleAntigravityVerifyBatch(w http.ResponseWriter, r *http.Request) {
	provider, _ := h.registry.ForType(AccountTypeAntigravity).(*AntigravityProvider)
	if provider == nil {
		respondJSONError(w, http.StatusServiceUnavailable, "Antigravity provider is not configured")
		return
	}
	results := make([]antigravityVerifyResult, 0)
	for _, account := range h.pool.allAccounts() {
		if account.Type != AccountTypeAntigravity {
			continue
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		result := h.verifyAntigravityAccount(ctx, account, provider.DailyURL(), provider.ProductionURL())
		cancel()
		h.auditAntigravityVerify(r, result)
		results = append(results, result)
	}
	respondJSON(w, map[string]any{"accounts": results})
}
