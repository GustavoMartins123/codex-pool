package main

import (
	"fmt"
	"log"
	"net/http"
	"strings"
	"time"
)

func upstreamTokenRevoked(resp *http.Response) bool {
	if resp == nil || resp.StatusCode != http.StatusUnauthorized {
		return false
	}
	for _, key := range []string{"X-Openai-Ide-Error-Code", "X-Openai-Ide-Root-Error-Code"} {
		if strings.EqualFold(strings.TrimSpace(resp.Header.Get(key)), "token_revoked") {
			return true
		}
	}
	return false
}

func codexAccessLive(a *Account, now time.Time) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	access := a.AccessToken
	a.mu.Unlock()
	if access == "" {
		return false
	}
	exp := parseCodexClaims(access).ExpiresAt
	return exp.IsZero() || exp.After(now)
}

// codexAccessFarFromExpiry reports whether the Codex access token is valid for
// well beyond now. A 401 alongside a still-fresh access JWT is not permission
// to spend the refresh token: OpenAI treats refresh reuse as "session ended",
// so burning it can log out sibling sessions that share the credential.
func codexAccessFarFromExpiry(a *Account, now time.Time) bool {
	if a == nil {
		return false
	}
	a.mu.Lock()
	defer a.mu.Unlock()
	if a.Type != AccountTypeCodex || a.ExpiresAt.IsZero() {
		return false
	}
	return a.ExpiresAt.After(now.Add(codexRefreshHeadroom))
}

func retireAfterRefreshFail(a *Account, err error, now time.Time) bool {
	if !isPermanentRefreshTokenError(err) {
		return false
	}
	if a != nil && a.Type == AccountTypeCodex && codexAccessLive(a, now) {
		return false
	}
	return true
}

// maxAuthErrorLength bounds the persisted credential rejection reason so a
// verbose upstream error body cannot bloat every account file.
const maxAuthErrorLength = 200

// markAccountNeedsLogin records a permanent refresh-token rejection (session
// ended, token reused or invalidated) on an account whose access token is
// still usable. The account keeps serving traffic, but the operator must
// relogin before the access token expires or the account goes dead silently.
func markAccountNeedsLogin(a *Account, reason string) {
	if a == nil {
		return
	}
	if len(reason) > maxAuthErrorLength {
		reason = reason[:maxAuthErrorLength]
	}

	a.mu.Lock()
	if a.NeedsLogin && a.AuthError == reason {
		a.mu.Unlock()
		return
	}
	a.NeedsLogin = true
	a.AuthError = reason
	accountID := a.ID
	a.mu.Unlock()

	log.Printf("account %s needs relogin: %s", accountID, reason)
	if err := saveAccount(a); err != nil {
		log.Printf("warning: failed to save needs-login state for account %s: %v", accountID, err)
	}
}

// clearAccountNeedsLogin drops the needs-login marker after the credentials
// recovered (successful refresh or operator relogin). Callers persist the
// account themselves; this only updates in-memory state.
func clearAccountNeedsLogin(a *Account) {
	if a == nil {
		return
	}
	a.mu.Lock()
	changed := a.NeedsLogin || a.AuthError != ""
	if changed {
		a.NeedsLogin = false
		a.AuthError = ""
	}
	a.mu.Unlock()
}

// persistDeadAccount retires an account once. Repeating the write is what made
// the pool watcher hot-reload every minute after a refresh token was revoked.
func persistDeadAccount(a *Account, reason string) {
	if a == nil {
		return
	}

	a.mu.Lock()
	alreadyDead := a.Dead
	a.Dead = true
	if !alreadyDead {
		a.Penalty += 100.0
	}
	accountID := a.ID
	a.mu.Unlock()

	if alreadyDead {
		return
	}

	log.Printf("marking account %s as dead: %s", accountID, reason)
	if err := saveAccount(a); err != nil {
		log.Printf("warning: failed to save dead account %s: %v", accountID, err)
	}
}

func accountUsesStaticAPIKey(accountType AccountType) bool {
	if isGenericProviderType(accountType) {
		return true
	}
	switch accountType {
	case AccountTypeKimi, AccountTypeMinimax, AccountTypeZAI, AccountTypeXiaomi, AccountTypeAdverserial, AccountTypeOpencodeGo:
		return true
	default:
		return false
	}
}

// applyProxyAuthFailure updates account state for 401/403 responses that came
// from proxied user requests. Codex requests can fail for request-scoped
// reasons, so they only get a small retry penalty here instead of being treated
// like a bad account.
func applyProxyAuthFailure(a *Account, refreshFailed bool) (markedDead bool, penaltyNow float64) {
	if a == nil {
		return false, 0
	}

	a.mu.Lock()
	defer a.mu.Unlock()

	if a.Type == AccountTypeCodex || accountUsesStaticAPIKey(a.Type) {
		// A proxied 401/403 is not a credential oracle for static-key providers:
		// model access and request shape can be rejected with the same status. Only
		// a provider-specific validation request may retire one of these accounts.
		a.Penalty += 0.2
		return false, a.Penalty
	}
	if refreshFailed {
		a.Dead = true
		a.Penalty += 1.0
		return true, a.Penalty
	}

	a.Penalty += 10.0
	return false, a.Penalty
}

// restoreValidatedAccount clears stale retirement state after a provider-specific
// credential check succeeds. The validation request is the authority for static
// API keys; a historical proxied 401 is not.
func restoreValidatedAccount(a *Account, validation string) {
	if a == nil {
		return
	}
	a.mu.Lock()
	wasDead := a.Dead
	if wasDead {
		a.Dead = false
		a.Penalty = 0
	}
	accountID := a.ID
	a.mu.Unlock()
	if !wasDead {
		return
	}
	log.Printf("restored account %s after successful %s validation", accountID, validation)
	if err := saveAccount(a); err != nil {
		log.Printf("warning: failed to persist restored account %s: %v", accountID, err)
	}
}

// disableAccountPermanently marks an account as permanently unavailable for
// routing and persists that state to disk.
func (h *proxyHandler) disableAccountPermanently(a *Account, reqID string, reason string) {
	if a == nil {
		return
	}

	a.mu.Lock()
	wasDisabled := a.Disabled
	a.Disabled = true
	a.Dead = true
	a.Penalty += 100.0
	// Keep the rejection reason so operator surfaces can explain that only a
	// relogin recovers this account.
	if !a.NeedsLogin {
		a.NeedsLogin = true
		a.AuthError = reason
	}
	accountID := a.ID
	a.mu.Unlock()

	prefix := ""
	if reqID != "" {
		prefix = fmt.Sprintf("[%s] ", reqID)
	}
	if wasDisabled {
		log.Printf("%saccount %s already disabled: %s", prefix, accountID, reason)
	} else {
		log.Printf("%sdisabling account %s permanently: %s", prefix, accountID, reason)
	}

	if err := saveAccount(a); err != nil {
		log.Printf("%swarning: failed to persist disabled account %s: %v", prefix, accountID, err)
	}
}
