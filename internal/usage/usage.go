// Package usage holds the per-request token consumption record shared by the
// providers, pricing and analytics.
package usage

import (
	"time"

	"codex-pool-proxy/internal/account"
)

// RequestUsage captures per-request token consumption parsed from SSE events.
type RequestUsage struct {
	PricingUnavailable  bool `json:"pricing_unavailable,omitempty"`
	Timestamp           time.Time
	AccountID           string
	PlanType            string
	UserID              string
	OriginID            string
	PromptCacheKey      string
	RequestID           string
	ProxyRequestID      string
	ClientCredentialID  string
	UsageSequence       int
	AttemptNumber       int
	UsageCompleteness   string
	InputTokens         int64
	CachedInputTokens   int64  // cache_read_input_tokens (cheap reads from cache)
	CacheCreationTokens int64  // cache_creation_input_tokens (expensive writes to cache)
	InputTokenMode      string // "inclusive" when InputTokens contains cached tokens; "exclusive" when it does not
	OutputTokens        int64
	ReasoningTokens     int64
	BillableTokens      int64
	// Rate limit snapshot after this request
	PrimaryUsedPct   float64
	SecondaryUsedPct float64
	// Reset metadata snapshots let analytics compare observed quota rollovers
	// with the schedule advertised when the request was served.
	PrimaryResetAt         time.Time
	SecondaryResetAt       time.Time
	PrimaryWindowMinutes   int
	SecondaryWindowMinutes int
	// Model and provider info
	Model       string              `json:"model,omitempty"`        // e.g., "claude-sonnet-4-5-20250929", "o4-mini"
	AccountType account.AccountType `json:"account_type,omitempty"` // "claude", "codex", "gemini"
}
