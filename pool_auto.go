package main

import (
	"fmt"
	"math"
	"sync"
)

// PoolAutoProfileWeights defines the signal weights for model orchestration.
type PoolAutoProfileWeights struct {
	HealthWeight      float64 `toml:"health" json:"health"`
	QuotaWeight       float64 `toml:"quota" json:"quota"`
	PerformanceWeight float64 `toml:"performance" json:"performance"`
	AdequacyWeight    float64 `toml:"adequacy" json:"adequacy"`
	AffinityWeight    float64 `toml:"affinity" json:"affinity"`
	CostWeight        float64 `toml:"cost" json:"cost"`
}

var defaultProfileWeights = map[string]PoolAutoProfileWeights{
	"balanced": {
		HealthWeight:      0.20,
		QuotaWeight:       0.20,
		PerformanceWeight: 0.20,
		AdequacyWeight:    0.20,
		AffinityWeight:    0.10,
		CostWeight:        0.10,
	},
	"fast": {
		HealthWeight:      0.15,
		QuotaWeight:       0.10,
		PerformanceWeight: 0.40,
		AdequacyWeight:    0.15,
		AffinityWeight:    0.10,
		CostWeight:        0.10,
	},
	"quality": {
		HealthWeight:      0.15,
		QuotaWeight:       0.15,
		PerformanceWeight: 0.10,
		AdequacyWeight:    0.45,
		AffinityWeight:    0.10,
		CostWeight:        0.05,
	},
	"efficient": {
		HealthWeight:      0.15,
		QuotaWeight:       0.30,
		PerformanceWeight: 0.10,
		AdequacyWeight:    0.10,
		AffinityWeight:    0.05,
		CostWeight:        0.30,
	},
	"long-context": {
		HealthWeight:      0.15,
		QuotaWeight:       0.15,
		PerformanceWeight: 0.10,
		AdequacyWeight:    0.40,
		AffinityWeight:    0.10,
		CostWeight:        0.10,
	},
}



// AutoScoreBreakdown captures the individual signal scores for explainability.
type AutoScoreBreakdown struct {
	HealthScore      float64 `json:"health_score"`
	QuotaScore       float64 `json:"quota_score"`
	PerformanceScore float64 `json:"performance_score"`
	AdequacyScore    float64 `json:"adequacy_score"`
	AffinityScore    float64 `json:"affinity_score"`
	CostScore        float64 `json:"cost_score"`
}

// AutoDecision contains the router's explainable decision for a pool/auto request.
type AutoDecision struct {
	Profile          string             `json:"profile"`
	SelectedModel    string             `json:"selected_model"`
	SelectedProvider AccountType        `json:"selected_provider"`
	Score            float64            `json:"score"`
	GatePassed       bool               `json:"gate_passed"`
	Scores           AutoScoreBreakdown `json:"scores"`
	Explanation      []string           `json:"explanation"`
	CandidateScores  map[string]float64 `json:"candidate_scores,omitempty"`
}

// PoolAutoOrchestrator manages multi-signal capacity-based model selection.
type PoolAutoOrchestrator struct {
	mu            sync.RWMutex
	customWeights map[string]PoolAutoProfileWeights
}

func newPoolAutoOrchestrator(customWeights map[string]PoolAutoProfileWeights) *PoolAutoOrchestrator {
	return &PoolAutoOrchestrator{
		customWeights: customWeights,
	}
}

func (o *PoolAutoOrchestrator) weightsForProfile(profile string) PoolAutoProfileWeights {
	if o != nil {
		o.mu.RLock()
		if w, ok := o.customWeights[profile]; ok {
			o.mu.RUnlock()
			return w
		}
		o.mu.RUnlock()
	}
	if w, ok := defaultProfileWeights[profile]; ok {
		return w
	}
	return defaultProfileWeights["balanced"]
}

// Candidate model pool for automatic selection across active providers.
var autoCandidateModels = []string{
	"gpt-6-astra",
	"gpt-5.6-sol",
	"gpt-5.6-terra",
	"gpt-5.6-luna",
	"gpt-5.4",
	"gpt-5.4-mini",
	"claude-opus-5",
	"claude-sonnet-5",
	"claude-sonnet-4-6",
	"claude-haiku-4-5-20251001",
	"antigravity/gemini-3.8-flash-high",
	"antigravity/gemini-3.8-pro",
	"gemini-3.7-flash",
	"gemini-3.5-flash",
	"k3",
	"MiniMax-M3",
	"glm-5.3",
	"mimo-v2.5-pro",
	"grok-4.6",
}

// Orchestrate selects the best model, provider, and account for a pool/auto request.
func (o *PoolAutoOrchestrator) Orchestrate(
	profile string,
	caps RequestCapabilities,
	conversationID string,
	pool *poolState,
	cb *CircuitBreakerManager,
	pricing *PricingData,
	metrics *metrics,
) (AutoDecision, error) {
	if profile == "" {
		profile = "balanced"
	}
	weights := o.weightsForProfile(profile)

	var bestModel string
	var bestMeta ModelMetadata
	var bestScore float64 = -1.0
	var bestScores AutoScoreBreakdown
	var bestExplanations []string

	candidateScores := make(map[string]float64)

	for _, candID := range autoCandidateModels {
		meta, ok := lookupModelMetadata(candID, pool)
		if !ok {
			continue
		}

		// SIGNAL 0: Mandatory Compatibility GATE
		// If model does not support a required capability, it receives score ZERO.
		compatible, _ := isModelCompatible(candID, caps, pool)
		if !compatible {
			candidateScores[candID] = 0.0
			continue
		}

		// SIGNAL 1: Health & Reliability (20%)
		healthScore := 1.0
		if cb != nil {
			provState := cb.State(providerKey(string(meta.Provider)))
			switch provState {
			case StateOpen:
				healthScore = 0.0
			case StateHalfOpen:
				healthScore = 0.5
			case StateClosed:
				healthScore = 1.0
			}
			if healthScore > 0 {
				modelState := cb.State(accountModelKey("", candID))
				if modelState == StateOpen {
					healthScore *= 0.2
				}
			}
		}
		if pool != nil {
			accounts := pool.accountsForType(meta.Provider)
			if len(accounts) == 0 {
				healthScore = 0.0
			} else {
				liveCount := 0
				for _, a := range accounts {
					a.mu.Lock()
					if !a.Dead && !a.Disabled {
						liveCount++
					}
					a.mu.Unlock()
				}
				healthScore *= float64(liveCount) / float64(len(accounts))
			}
		}
		if healthScore <= 0 {
			candidateScores[candID] = 0.0
			continue
		}

		// SIGNAL 2: Projected Quota (20%)
		quotaScore := 0.70 // default healthy baseline
		if pool != nil {
			accounts := pool.accountsForType(meta.Provider)
			if len(accounts) > 0 {
				var totalHeadroom float64
				for _, a := range accounts {
					a.mu.Lock()
					used := a.Usage.SecondaryUsedPercent
					if used == 0 && a.Usage.PrimaryUsedPercent > 0 {
						used = a.Usage.PrimaryUsedPercent
					}
					totalHeadroom += math.Max(0, 1.0-used)
					a.mu.Unlock()
				}
				quotaScore = totalHeadroom / float64(len(accounts))
			}
		}

		// SIGNAL 3: Performance (20%)
		perfScore := 0.80
		switch candID {
		case "gpt-5.6-luna", "gpt-5.4-mini", "claude-haiku-4-5-20251001", "gemini-3.7-flash", "gemini-3.5-flash", "antigravity/gemini-3.8-flash-high":
			perfScore = 1.0
		case "gpt-5.6-terra", "claude-sonnet-5", "claude-sonnet-4-6", "MiniMax-M3", "glm-5.3", "k3":
			perfScore = 0.85
		case "gpt-6-astra", "claude-opus-5":
			perfScore = 0.65
		}

		// SIGNAL 4: Model Adequacy / Quality (20%)
		adequacyScore := 0.80
		switch profile {
		case "quality":
			switch candID {
			case "gpt-6-astra", "claude-opus-5", "claude-sonnet-5", "gpt-5.6-sol":
				adequacyScore = 1.0
			case "gpt-5.6-terra", "antigravity/gemini-3.8-pro", "MiniMax-M3", "k3":
				adequacyScore = 0.80
			default:
				adequacyScore = 0.40
			}
		case "fast":
			switch candID {
			case "gpt-5.6-luna", "gpt-5.4-mini", "gemini-3.7-flash", "antigravity/gemini-3.8-flash-high", "claude-haiku-4-5-20251001":
				adequacyScore = 1.0
			case "gpt-5.6-terra", "claude-sonnet-4-6":
				adequacyScore = 0.70
			default:
				adequacyScore = 0.40
			}
		case "efficient":
			switch candID {
			case "gemini-3.7-flash", "gpt-5.6-luna", "gpt-5.4-mini", "antigravity/gemini-3.8-flash-high", "glm-5.3", "mimo-v2.5-pro":
				adequacyScore = 1.0
			case "gpt-5.6-terra", "claude-haiku-4-5-20251001":
				adequacyScore = 0.80
			default:
				adequacyScore = 0.40
			}
		case "long-context":
			if meta.ContextWindow >= 1000000 {
				adequacyScore = 1.0
			} else {
				adequacyScore = 0.20
			}
		default: // balanced
			switch candID {
			case "gpt-5.6-sol", "claude-sonnet-5", "gemini-3.7-flash", "antigravity/gemini-3.8-flash-high":
				adequacyScore = 1.0
			case "gpt-6-astra", "gpt-5.6-terra", "MiniMax-M3", "k3":
				adequacyScore = 0.85
			default:
				adequacyScore = 0.70
			}
		}

		// SIGNAL 5: Affinity / Context Reuse (10%)
		affinityScore := 0.30
		if conversationID != "" && pool != nil {
			pool.mu.Lock()
			pinnedID := pool.convPin[conversationID]
			pool.mu.Unlock()
			if pinnedID != "" {
				for _, a := range pool.accounts {
					if a.ID == pinnedID && a.Type == meta.Provider {
						affinityScore = 1.0 // Strong session continuity
						break
					}
				}
			}
		}

		// SIGNAL 6: Estimated Economic Cost (10%)
		costScore := 0.70
		if pricing != nil {
			if pr, ok := pricing.lookupPricing(candID); ok {
				blendedCost := (pr.InputCostPerToken*1e6 + pr.OutputCostPerToken*1e6) / 2.0
				if blendedCost <= 1.0 {
					costScore = 1.0
				} else if blendedCost <= 5.0 {
					costScore = 0.85
				} else if blendedCost <= 15.0 {
					costScore = 0.60
				} else {
					costScore = 0.30
				}
			}
		}

		// Total Weighted Score Calculation
		totalScore := (weights.HealthWeight * healthScore) +
			(weights.QuotaWeight * quotaScore) +
			(weights.PerformanceWeight * perfScore) +
			(weights.AdequacyWeight * adequacyScore) +
			(weights.AffinityWeight * affinityScore) +
			(weights.CostWeight * costScore)

		candidateScores[candID] = totalScore

		if totalScore > bestScore {
			bestScore = totalScore
			bestModel = candID
			bestMeta = meta
			bestScores = AutoScoreBreakdown{
				HealthScore:      healthScore,
				QuotaScore:       quotaScore,
				PerformanceScore: perfScore,
				AdequacyScore:    adequacyScore,
				AffinityScore:    affinityScore,
				CostScore:        costScore,
			}
			bestExplanations = []string{
				"compatibility_gate_passed",
				fmt.Sprintf("profile:%s", profile),
				fmt.Sprintf("health_score:%.2f", healthScore),
				fmt.Sprintf("quota_score:%.2f", quotaScore),
				fmt.Sprintf("performance_score:%.2f", perfScore),
				fmt.Sprintf("adequacy_score:%.2f", adequacyScore),
				fmt.Sprintf("affinity_score:%.2f", affinityScore),
				fmt.Sprintf("cost_score:%.2f", costScore),
			}
		}
	}

	if bestModel == "" || bestScore <= 0 {
		return AutoDecision{}, fmt.Errorf("no compatible and healthy model found for pool/auto profile %q", profile)
	}

	return AutoDecision{
		Profile:          profile,
		SelectedModel:    bestModel,
		SelectedProvider: bestMeta.Provider,
		Score:            bestScore,
		GatePassed:       true,
		Scores:           bestScores,
		Explanation:      bestExplanations,
		CandidateScores:  candidateScores,
	}, nil
}
