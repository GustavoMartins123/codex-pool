package main

import (
	"strings"
)

// ResetWindowKind is the provider-independent name for a quota window. The
// upstream APIs expose these windows in different slots (or do not expose a
// second window at all), so the UI must not infer their meaning from position.
type ResetWindowKind string

const (
	ResetWindowNone      ResetWindowKind = "none"
	ResetWindowFiveHour  ResetWindowKind = "five_hour"
	ResetWindowDaily     ResetWindowKind = "daily"
	ResetWindowWeekly    ResetWindowKind = "weekly"
	ResetWindowTokens    ResetWindowKind = "tokens"
	ResetWindowRequests  ResetWindowKind = "requests"
	ResetWindowPrimary   ResetWindowKind = "primary"
	ResetWindowSecondary ResetWindowKind = "secondary"
)

// AccountTier is the normalized subscription tier used when a provider's
// quota shape depends on the account plan. It intentionally stays separate
// from RateLimitTier, which is an upstream throttling class (for example,
// Claude's 5x/20x tiers).
type AccountTier string

const (
	AccountTierUnknown     AccountTier = "unknown"
	AccountTierPlus        AccountTier = "plus"
	AccountTierTeamBasic   AccountTier = "team_basic"
	AccountTierProOrHigher AccountTier = "pro_or_higher"
)

// ResetWindowPolicy is the canonical quota shape for one provider account.
// Primary and Secondary refer to the normalized API fields, not to the
// positional slots used by a particular upstream.
type ResetWindowPolicy struct {
	Tier      AccountTier     `json:"tier"`
	Primary   ResetWindowKind `json:"primary"`
	Secondary ResetWindowKind `json:"secondary"`
}

func normalizePlanName(plan string) string {
	plan = strings.ToLower(strings.TrimSpace(plan))
	plan = strings.NewReplacer("_", " ", "-", " ", "/", " ").Replace(plan)
	return strings.Join(strings.Fields(plan), " ")
}

func codexAccountTier(plan string) AccountTier {
	normalized := normalizePlanName(plan)
	switch {
	case strings.Contains(normalized, "plus"):
		return AccountTierPlus
	case strings.Contains(normalized, "team"):
		// The Codex team plan shown by the account service is Team Basic. Keep
		// accepting the short "team" value used by older auth claims.
		return AccountTierTeamBasic
	case strings.Contains(normalized, "pro"), strings.Contains(normalized, "enterprise"),
		strings.Contains(normalized, "max"):
		return AccountTierProOrHigher
	default:
		return AccountTierUnknown
	}
}

// resetWindowPolicy is the single provider/tier mapping used by the API and
// frontend. Add provider-specific shapes here instead of adding provider name
// conditionals to presentation code.
func resetWindowPolicy(provider AccountType, plan string) ResetWindowPolicy {
	switch provider {
	case AccountTypeCodex:
		tier := codexAccountTier(plan)
		switch tier {
		case AccountTierPlus, AccountTierTeamBasic:
			return ResetWindowPolicy{Tier: tier, Primary: ResetWindowFiveHour, Secondary: ResetWindowWeekly}
		case AccountTierProOrHigher:
			return ResetWindowPolicy{Tier: AccountTierProOrHigher, Primary: ResetWindowNone, Secondary: ResetWindowWeekly}
		default:
			// Preserve the historical shape for an unclassified Codex claim. A
			// live duration still remains the source of truth for availability.
			return ResetWindowPolicy{Tier: AccountTierUnknown, Primary: ResetWindowFiveHour, Secondary: ResetWindowWeekly}
		}
	case AccountTypeAntigravity:
		return ResetWindowPolicy{Tier: AccountTierUnknown, Primary: ResetWindowFiveHour, Secondary: ResetWindowWeekly}
	case AccountTypeZAI:
		// Z.ai's response headers describe request and token rate limits. They
		// do not establish a daily or weekly Coding Plan quota window.
		return ResetWindowPolicy{Tier: AccountTierUnknown, Primary: ResetWindowRequests, Secondary: ResetWindowTokens}
	case AccountTypeClaude:
		return ResetWindowPolicy{Tier: AccountTierUnknown, Primary: ResetWindowTokens, Secondary: ResetWindowRequests}
	case AccountTypeGemini:
		return ResetWindowPolicy{Tier: AccountTierUnknown, Primary: ResetWindowDaily, Secondary: ResetWindowNone}
	default:
		return ResetWindowPolicy{Tier: AccountTierUnknown, Primary: ResetWindowPrimary, Secondary: ResetWindowSecondary}
	}
}

func resetWindowLabel(kind ResetWindowKind) string {
	switch kind {
	case ResetWindowFiveHour:
		return "5 hour"
	case ResetWindowDaily:
		return "Daily"
	case ResetWindowWeekly:
		return "Weekly"
	case ResetWindowTokens:
		return "Tokens"
	case ResetWindowRequests:
		return "Requests"
	case ResetWindowSecondary:
		return "Secondary"
	case ResetWindowNone:
		return ""
	default:
		return "Primary"
	}
}
