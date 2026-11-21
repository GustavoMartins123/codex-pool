package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"strings"
	"time"
)

// accountVerifyResult is the on-demand access check answer. Every probe is a
// zero-cost read (quota, usage or model list), never a generation request.
type accountVerifyResult struct {
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

func accountVerifyDetail(text string) string {
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

func (h *proxyHandler) verifyAntigravityAccount(ctx context.Context, acc *Account, daily, production *url.URL) accountVerifyResult {
	result := accountVerifyResult{AccountID: acc.ID, CheckedAt: time.Now().UTC().Format(time.RFC3339)}
	fail := func(status, detail string) accountVerifyResult {
		result.Status = status
		result.Detail = accountVerifyDetail(detail)
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
				persistDeadAccount(h.pool.antigravity, acc, "refresh token revoked/invalid")
				return fail("revoked", err.Error())
			}
			return fail("error", err.Error())
		}
	}

	summary, err := fetchAntigravityQuotaSummary(ctx, h.pool.antigravity, h.transport, acc, daily, production)
	var forbidden *antigravityForbiddenError
	if errors.As(err, &forbidden) {
		recordAntigravityForbidden(h.pool.antigravity, acc, forbidden.body)
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

	clearAntigravityHealth(h.pool.antigravity, acc)
	if antigravityQuotaExhausted(summary) {
		return fail("quota_exhausted", "all enabled quota buckets are exhausted")
	}
	result.Status = "healthy"
	return result
}

// accountVerifySupported reports whether the provider exposes a probe that
// authenticates the account without spending quota. Providers without one stay
// unverifiable instead of reporting a health signal they cannot produce.
func accountVerifySupported(accType AccountType) bool {
	switch accType {
	case AccountTypeAntigravity, AccountTypeCodex, AccountTypeClaude, AccountTypeGemini,
		AccountTypeGrok, AccountTypeKimi, AccountTypeMinimax, AccountTypeZAI,
		AccountTypeOpencodeGo, AccountTypeQwen, AccountTypeAIStudio:
		return true
	default:
		return false
	}
}

func (h *proxyHandler) verifyAccountAccess(ctx context.Context, acc *Account) accountVerifyResult {
	now := time.Now().UTC()
	result := accountVerifyResult{AccountID: acc.ID, CheckedAt: now.Format(time.RFC3339)}
	if acc.Type == AccountTypeAntigravity {
		provider, _ := h.registry.ForType(AccountTypeAntigravity).(*AntigravityProvider)
		if provider == nil {
			result.Status = "error"
			result.Detail = "Antigravity provider is not configured"
			return result
		}
		return h.verifyAntigravityAccount(ctx, acc, provider.DailyURL(), provider.ProductionURL())
	}

	probeErr := h.probeAccountAccess(ctx, acc, now)
	message := ""
	if probeErr != nil {
		message = probeErr.Error()
	}

	acc.mu.Lock()
	defer acc.mu.Unlock()
	detail := message
	switch {
	case acc.NeedsVerification || strings.TrimSpace(acc.VerificationURL) != "":
		result.Status = "needs_verification"
		result.VerificationURL = safeAntigravityAppealURL(acc.VerificationURL)
	case strings.TrimSpace(acc.HealthError) != "":
		result.Status = "error"
		detail = acc.HealthError
	case acc.Dead:
		result.Status = "revoked"
		if acc.AuthError != "" {
			detail = acc.AuthError
		}
	case accountCoolingDownLocked(acc, now):
		// Several usage fetchers swallow 429s and return nil, so the cooldown
		// is the only evidence that the probe was rate limited.
		result.Status = "rate_limited"
		detail = fmt.Sprintf("cooling down until %s", acc.RateLimitUntil.UTC().Format(time.RFC3339))
	case probeErr != nil:
		result.Status = "error"
	case accountUsageExhaustedLocked(h.pool.antigravity, acc):
		result.Status = "quota_exhausted"
		detail = "usage windows are exhausted"
	default:
		result.Status = "healthy"
		switch {
		case acc.Type == AccountTypeGemini:
			detail = "Gemini exposes no usage endpoint; the access token is still valid"
		case !acc.Usage.RetrievedAt.IsZero():
			source := acc.Usage.Source
			if source == "" {
				source = "usage"
			}
			detail = fmt.Sprintf("%s: primary %.0f%% used, secondary %.0f%% used", source,
				accountPrimaryUsageLocked(acc)*100, accountSecondaryUsageLocked(acc)*100)
		default:
			detail = "credentials accepted by the provider"
		}
	}
	result.Detail = accountVerifyDetail(detail)
	return result
}

// probeAccountAccess reuses the poller's zero-cost probes. Model discovery is
// the fallback for providers whose only authenticated read is the models list;
// MiniMax in particular must never be probed with its generation-based seed.
func (h *proxyHandler) probeAccountAccess(ctx context.Context, acc *Account, now time.Time) error {
	switch acc.Type {
	case AccountTypeCodex:
		return h.fetchUsage(now, acc)
	case AccountTypeClaude:
		acc.mu.Lock()
		oauth := strings.HasPrefix(acc.AccessToken, "sk-ant-oat")
		acc.mu.Unlock()
		if oauth {
			return h.fetchClaudeUsage(now, acc)
		}
	case AccountTypeGrok:
		return h.fetchGrokUsage(now, acc)
	case AccountTypeKimi:
		return h.fetchKimiUsage(now, acc)
	case AccountTypeZAI:
		return h.syncZAIUsage(now, acc)
	case AccountTypeOpencodeGo:
		return h.fetchOpencodeGoUsage(now, acc)
	case AccountTypeGemini:
		return h.verifyGeminiToken(ctx, acc, now)
	}
	return syncProviderModels(ctx, h.pool.antigravity, h.transport, h.registry, acc)
}

func (h *proxyHandler) verifyGeminiToken(ctx context.Context, acc *Account, now time.Time) error {
	acc.mu.Lock()
	live := !acc.ExpiresAt.IsZero() && acc.ExpiresAt.After(now.Add(5*time.Minute))
	acc.mu.Unlock()
	if live {
		return nil
	}
	err := h.refreshAccount(ctx, acc)
	if err != nil && isPermanentRefreshTokenError(err) {
		persistDeadAccount(h.pool.antigravity, acc, "refresh token revoked/invalid")
	}
	return err
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
	if !accountVerifySupported(target.Type) {
		respondJSONError(w, http.StatusBadRequest, fmt.Sprintf("access verification is not available for %s accounts", target.Type))
		return
	}
	if h.registry.ForType(target.Type) == nil {
		respondJSONError(w, http.StatusServiceUnavailable, fmt.Sprintf("%s provider is not configured", target.Type))
		return
	}
	ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
	defer cancel()
	result := h.verifyAccountAccess(ctx, target)
	h.recordAdminAudit(r, "account.verify", result.AccountID, result.Status)
	respondJSON(w, result)
}

func (h *proxyHandler) handleAccountVerifyBatch(w http.ResponseWriter, r *http.Request) {
	actor := providerContributionActor(r)
	results := make([]accountVerifyResult, 0)
	for _, account := range h.pool.allAccounts() {
		if !accountVerifySupported(account.Type) {
			continue
		}
		if h.passport != nil && h.passport.authorizeAccount(actor, account, "manage") != nil {
			continue
		}
		ctx, cancel := context.WithTimeout(r.Context(), 30*time.Second)
		result := h.verifyAccountAccess(ctx, account)
		cancel()
		h.recordAdminAudit(r, "account.verify", result.AccountID, result.Status)
		results = append(results, result)
	}
	respondJSON(w, map[string]any{"accounts": results})
}
