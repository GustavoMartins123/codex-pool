package routing

import "time"

// QuotaSignals is the snapshot used for quota-based account scoring.
type QuotaSignals struct {
	PrimaryUsed, SecondaryUsed              float64
	PrimaryAvailable, SecondaryAvailable    bool
	PrimaryResetAt, SecondaryResetAt        time.Time
	ExpiresAt, LastUsed                     time.Time
	Penalty                                 float64
	PrimaryBurst, HasCredits, HealthBlocked bool
}

// QuotaScore describes the components of a quota-based account score.
type QuotaScore struct {
	Score              float64
	PrimaryUsed        float64
	SecondaryUsed      float64
	PrimaryAvailable   bool
	SecondaryAvailable bool
	BaseWindow         string
	BaseHeadroom       float64
	DrainMultiplier    float64
	PrimaryPaceBonus   float64
	PrimaryPenalty     float64
	ExpiryPenalty      float64
	PenaltyRaw         float64
	PenaltyFactor      float64
	PenaltyApplied     float64
	ClampedToFloor     bool
	RecentUseBonus     float64
	CreditBonus        float64
	HeadroomPreCredit  float64
}

// ScoreQuota calculates the tiered policy score for a quota snapshot.
func ScoreQuota(signals QuotaSignals, now time.Time) QuotaScore {
	var out QuotaScore
	primaryUsed, secondaryUsed := signals.PrimaryUsed, signals.SecondaryUsed
	out.PrimaryUsed, out.SecondaryUsed = primaryUsed, secondaryUsed
	out.PrimaryAvailable, out.SecondaryAvailable = signals.PrimaryAvailable, signals.SecondaryAvailable

	// Prefer the weekly window as the long-term balancing signal. If OpenAI
	// temporarily exposes only a five-hour window, use that instead of treating
	// the missing weekly window as unlimited headroom.
	headroom := 1.0
	switch {
	case out.SecondaryAvailable:
		headroom = 1.0 - secondaryUsed
		out.BaseWindow = "7d"
	case out.PrimaryAvailable:
		headroom = 1.0 - primaryUsed
		out.BaseWindow = "5h"
	default:
		out.BaseWindow = "none"
	}
	out.BaseHeadroom = headroom
	out.DrainMultiplier = 1.0

	// Accounts closer to a weekly reset can absorb more traffic right now.
	if out.SecondaryAvailable && !signals.SecondaryResetAt.IsZero() && signals.SecondaryResetAt.After(now) {
		hoursRemaining := signals.SecondaryResetAt.Sub(now).Hours()
		totalHours := 168.0
		if hoursRemaining > 1 && hoursRemaining < totalHours {
			sustainableBurnRate := headroom / hoursRemaining
			baselineBurnRate := 1.0 / totalHours
			burnRateRatio := sustainableBurnRate / baselineBurnRate

			maxMultiplier := 3.0
			if hoursRemaining < 6 && headroom > 0.1 {
				maxMultiplier = 8.0
			}

			if burnRateRatio > maxMultiplier {
				burnRateRatio = maxMultiplier
			} else if burnRateRatio < 0.3 {
				burnRateRatio = 0.3
			}

			out.DrainMultiplier = burnRateRatio
			headroom *= burnRateRatio
		}
	}

	// Cap drain multiplier for accounts with no primary window data.
	// Pro/Team plans lack a 5hr window; don't let phantom headroom inflate their score.
	if !out.PrimaryAvailable {
		if out.DrainMultiplier > 1.0 {
			out.DrainMultiplier = 1.0
			headroom = out.BaseHeadroom
		}
	}

	// When both windows exist, use five-hour headroom as the burst signal on
	// top of the weekly base. If five-hour is the only window, it is already the
	// base and must not be counted twice. Grok's Primary slot is monthly API
	// spend (display-only, not a burst window), so it neither bonuses nor
	// penalizes the score; weekly credits drive it.
	if signals.PrimaryBurst && out.PrimaryAvailable && out.SecondaryAvailable {
		if !signals.PrimaryResetAt.IsZero() && signals.PrimaryResetAt.After(now) {
			hoursRemaining := signals.PrimaryResetAt.Sub(now).Hours()
			primaryHeadroom := 1.0 - primaryUsed
			if hoursRemaining > 0.1 && hoursRemaining <= 5.0 && primaryHeadroom > 0.05 {
				timeWeight := hoursRemaining / 3.0
				if timeWeight > 1.0 {
					timeWeight = 1.0
				}
				out.PrimaryPaceBonus = primaryHeadroom * 0.5 * timeWeight
			}
		} else if primaryUsed < 0.5 {
			out.PrimaryPaceBonus = (1.0 - primaryUsed) * 0.15
		}
		headroom += out.PrimaryPaceBonus

		if primaryUsed > 0.8 {
			out.PrimaryPenalty = (primaryUsed - 0.8) * 2.0
			headroom -= out.PrimaryPenalty
		}
	}

	// Mild expiry penalty.
	if !signals.ExpiresAt.IsZero() {
		ttl := signals.ExpiresAt.Sub(now).Minutes()
		if ttl < 0 {
			out.ExpiryPenalty = 0.3
		} else if ttl < 30 {
			out.ExpiryPenalty = 0.2
		} else if ttl < 60 {
			out.ExpiryPenalty = 0.1
		}
	}
	headroom -= out.ExpiryPenalty

	out.PenaltyFactor = 1.0
	if !signals.SecondaryResetAt.IsZero() {
		hoursRemaining := signals.SecondaryResetAt.Sub(now).Hours()
		secondaryHeadroom := 1.0 - secondaryUsed
		if hoursRemaining > 0 && hoursRemaining < 6 && secondaryHeadroom > 0.1 {
			out.PenaltyFactor = 0.3
		}
	}
	out.PenaltyRaw = signals.Penalty
	out.PenaltyApplied = signals.Penalty * out.PenaltyFactor
	headroom -= out.PenaltyApplied

	if headroom < 0.01 {
		headroom = 0.01
		out.ClampedToFloor = true
	}

	if !signals.LastUsed.IsZero() && now.Sub(signals.LastUsed) < 5*time.Minute {
		out.RecentUseBonus = 0.1
		headroom += out.RecentUseBonus
	}

	out.CreditBonus = 1.0
	if signals.HasCredits {
		out.CreditBonus = 1.1
	}

	out.HeadroomPreCredit = headroom
	out.Score = headroom * out.CreditBonus
	if signals.HealthBlocked {
		out.Score = 0
	}
	return out
}
