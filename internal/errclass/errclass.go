// Package errclass classifies upstream HTTP failures for the retry loop.
package errclass

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
)

// Class categorises an upstream HTTP response or error so the retry loop
// can decide what to do: retry on another account, back off, give up, etc.
type Class int

const (
	// None means no error — the request succeeded.
	None Class = iota

	// Transient covers temporary server-side failures (408, 500-504).
	// Action: retry immediately on the next account.
	Transient

	// RateLimit means 429 Too Many Requests.
	// Action: exponential backoff on the account, then try another.
	RateLimit

	// Auth covers 401/403 — token expired or revoked.
	// Action: refresh token, retry same account once; if still failing, rotate.
	Auth

	// Payment means 402 — subscription lapsed, workspace deactivated.
	// Action: mark account dead, rotate.
	Payment

	// NotFound means 404 — model not available on this account.
	// Action: rotate to another account (don't penalise heavily).
	NotFound

	// Invalid means 400 — the request itself is bad.
	// Action: return error to client, do NOT retry.
	Invalid

	// Fatal is a catch-all for non-retryable errors we don't have
	// a specific category for. Action: return error to client.
	Fatal
)

// Classify maps an HTTP status code to a Class.
func Classify(statusCode int) Class {
	switch {
	case statusCode >= 200 && statusCode < 400:
		return None

	case statusCode == http.StatusBadRequest: // 400
		return Invalid

	case statusCode == http.StatusUnauthorized, // 401
		statusCode == http.StatusForbidden: // 403
		return Auth

	case statusCode == http.StatusPaymentRequired: // 402
		return Payment

	case statusCode == http.StatusNotFound: // 404
		return NotFound

	case statusCode == http.StatusRequestTimeout: // 408
		return Transient

	case statusCode == 524: // Cloudflare timeout — origin didn't respond in time
		return Transient

	case statusCode == http.StatusTooManyRequests: // 429
		return RateLimit

	case statusCode == 529: // Anthropic overloaded — retry elsewhere without cooling down the account
		return Transient

	case statusCode >= 500 && statusCode <= 599:
		return Transient

	default:
		return Fatal
	}
}

// IsDeactivatedWorkspace checks the body for signs that the account is
// permanently dead (deactivated workspace, cancelled subscription, etc.).
func IsDeactivatedWorkspace(body []byte) bool {
	s := strings.ToLower(string(body))
	return strings.Contains(s, "deactivated_workspace") ||
		strings.Contains(s, "subscription") ||
		strings.Contains(s, "billing") ||
		strings.Contains(s, "payment_required")
}

// IsClaudeOrganizationDisabled checks for Anthropic responses that indicate
// the org behind a Claude OAuth token has been disabled permanently.
func IsClaudeOrganizationDisabled(body []byte) bool {
	s := strings.ToLower(string(body))
	return strings.Contains(s, "organization has been disabled") ||
		strings.Contains(s, "organization is disabled") ||
		strings.Contains(s, "organization_has_been_disabled") ||
		strings.Contains(s, "organization_disabled") ||
		strings.Contains(s, "oauth authentication is currently not allowed")
}

// IsCloudflareChallenge detects Cloudflare bot-mitigation responses that return
// 403 with an HTML challenge page. These are transient (not auth failures) and
// should not penalize accounts.
func IsCloudflareChallenge(body []byte, headers http.Header) bool {
	if headers.Get("Cf-Mitigated") == "challenge" {
		return true
	}
	if headers.Get("Server") == "cloudflare" && strings.Contains(string(body), "<html") {
		return true
	}
	return false
}

func IsCyberPolicy(body []byte) bool {
	// Only match the upstream's actual error envelope. Naive substring
	// matching trips on any assistant output that happens to contain the
	// literal phrase "cyber_policy" — e.g. an assistant explaining this
	// very codebase — which would silently replace its reply with a
	// synthetic refusal. Parse the JSON shape and require code or type
	// to equal the sentinel.
	if !bytes.Contains(body, []byte(`"cyber_policy"`)) {
		return false
	}
	trimmed := bytes.TrimSpace(body)
	if len(trimmed) == 0 || trimmed[0] != '{' {
		return false
	}
	var env struct {
		Type  string `json:"type"`
		Code  string `json:"code"`
		Error struct {
			Type string `json:"type"`
			Code string `json:"code"`
		} `json:"error"`
	}
	if err := json.Unmarshal(trimmed, &env); err != nil {
		return false
	}
	return env.Code == "cyber_policy" || env.Type == "cyber_policy" ||
		env.Error.Code == "cyber_policy" || env.Error.Type == "cyber_policy"
}

func IsCodexModelUnavailable(body []byte) bool {
	s := strings.ToLower(string(body))
	return strings.Contains(s, "model_not_found") ||
		strings.Contains(s, "model not found") ||
		strings.Contains(s, "model_not_supported") ||
		strings.Contains(s, "model not supported") ||
		strings.Contains(s, "model is not supported") ||
		strings.Contains(s, "not available for") ||
		strings.Contains(s, "does not have access to model")
}

// IsCodexItemReference detects the upstream 404 for input items that
// reference previously stored responses ("Item with id 'rs_...' not found.
// Items are not persisted when `store` is set to false."). The pool must
// forward store=false, so these references can never resolve; the failure
// belongs to the request payload, not to the account, and must not rotate
// accounts or feed the circuit breaker.
func IsCodexItemReference(body []byte) bool {
	s := strings.ToLower(string(body))
	return strings.Contains(s, "item with id") && strings.Contains(s, "not found") ||
		strings.Contains(s, "items are not persisted")
}

// Retryable returns true if this class should be retried on another account.
func (c Class) Retryable() bool {
	switch c {
	case Transient, RateLimit, Auth,
		Payment, NotFound:
		return true
	default:
		return false
	}
}

// String returns a human-readable label.
func (c Class) String() string {
	switch c {
	case None:
		return "none"
	case Transient:
		return "transient"
	case RateLimit:
		return "rate_limit"
	case Auth:
		return "auth"
	case Payment:
		return "payment"
	case NotFound:
		return "not_found"
	case Invalid:
		return "invalid"
	case Fatal:
		return "fatal"
	default:
		return "unknown"
	}
}

func IsRateLimit(err error) bool {
	if err == nil {
		return false
	}
	msg := strings.ToLower(err.Error())
	return strings.Contains(msg, "rate limited") || strings.Contains(msg, "too many requests") || strings.Contains(msg, "429")
}
