package routing

import "sort"

const competitiveScoreWindow = 0.15

// TierCandidate is an eligible account with its tier and quota score.
type TierCandidate[A any] struct {
	Account      A
	ID           string
	Tier         int
	SecondaryUse float64
	Score        float64
	CyberAccess  bool
}

// PickTiered ranks eligible accounts. The input order breaks equal scores for
// unweighted pools, while Codex shares competitive capacity by account ID.
func PickTiered[A any](candidates []TierCandidate[A], threshold float64, codex bool, rotation uint64) (A, bool) {
	var tiers [3]struct {
		best  *TierCandidate[A]
		below *TierCandidate[A]
	}
	for i := range candidates {
		candidate := &candidates[i]
		if candidate.Tier < 1 || candidate.Tier > len(tiers) {
			continue
		}
		tier := &tiers[candidate.Tier-1]
		if tier.best == nil || candidate.Score > tier.best.Score {
			tier.best = candidate
		}
		if candidate.SecondaryUse < threshold && (tier.below == nil || candidate.Score > tier.below.Score) {
			tier.below = candidate
		}
	}

	var selected *TierCandidate[A]
	belowThreshold := false
	for _, tier := range tiers {
		if tier.below != nil {
			selected = tier.below
			belowThreshold = true
			break
		}
		if tier.best != nil {
			selected = tier.best
			break
		}
	}
	if selected == nil {
		var zero A
		return zero, false
	}
	if !codex {
		if second := tiers[1].below; selected.Tier == 1 && !belowThreshold && second != nil && second.Score > selected.Score+0.3 {
			return second.Account, true
		}
		return selected.Account, true
	}

	competitive := make([]*TierCandidate[A], 0, len(candidates))
	minimumScore := selected.Score - competitiveScoreWindow
	totalWeight := 0
	for i := range candidates {
		candidate := &candidates[i]
		if candidate.Tier == selected.Tier && (!belowThreshold || candidate.SecondaryUse < threshold) && candidate.Score >= minimumScore {
			competitive = append(competitive, candidate)
			totalWeight += candidateWeight(candidate.CyberAccess)
		}
	}
	sort.Slice(competitive, func(i, j int) bool {
		return competitive[i].ID < competitive[j].ID
	})
	if totalWeight > 0 {
		slot := int(rotation % uint64(totalWeight))
		for _, candidate := range competitive {
			weight := candidateWeight(candidate.CyberAccess)
			if slot < weight {
				return candidate.Account, true
			}
			slot -= weight
		}
	}
	var zero A
	return zero, false
}

func candidateWeight(cyberAccess bool) int {
	if cyberAccess {
		return 2
	}
	return 1
}
