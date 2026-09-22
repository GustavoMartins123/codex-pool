package main

import (
	"fmt"
	"strings"
	"sync"
	"time"
)

// FallbackTrigger specifies the condition that triggered model fallback.
type FallbackTrigger string

const (
	Trigger429         FallbackTrigger = "on_429"
	TriggerUnavailable FallbackTrigger = "on_unavailable"
)

// FallbackRule holds the candidate fallback models for different failure conditions.
type FallbackRule struct {
	On429         []string `json:"on_429"`
	OnUnavailable []string `json:"on_unavailable"`
}

// FallbackGraph manages model fallback routing with capability-aware validation.
type FallbackGraph struct {
	mu     sync.RWMutex
	routes map[string]FallbackRule
}

func newFallbackGraph() *FallbackGraph {
	fg := &FallbackGraph{
		routes: make(map[string]FallbackRule),
	}
	fg.initDefaultRoutes()
	return fg
}

func (fg *FallbackGraph) initDefaultRoutes() {
	fg.routes = map[string]FallbackRule{
		"gpt-6-astra": {
			On429:         []string{"gpt-5.6-sol", "claude-opus-5", "claude-sonnet-5"},
			OnUnavailable: []string{"gpt-5.6-sol", "claude-sonnet-5", "gemini-3.7-flash"},
		},
		"gpt-5.6-sol": {
			On429:         []string{"gpt-6-astra", "claude-sonnet-5", "antigravity/gemini-3.8-flash-high"},
			OnUnavailable: []string{"claude-sonnet-5", "gemini-3.7-flash", "antigravity/gemini-3.8-flash-high"},
		},
		"gpt-5.6-terra": {
			On429:         []string{"gpt-5.6-luna", "gemini-3.7-flash", "claude-sonnet-4-6"},
			OnUnavailable: []string{"gpt-5.6-luna", "gemini-3.7-flash", "k3"},
		},
		"gpt-5.6-luna": {
			On429:         []string{"gpt-5.4-mini", "gemini-3.5-flash", "claude-haiku-4-5"},
			OnUnavailable: []string{"gemini-3.5-flash", "claude-haiku-4-5", "gpt-5.4-mini"},
		},
		"gpt-5.4": {
			On429:         []string{"gpt-5.6-sol", "gpt-5.6-terra", "claude-sonnet-5"},
			OnUnavailable: []string{"gpt-5.6-terra", "claude-sonnet-5", "gemini-3.7-flash"},
		},
		"claude-sonnet-5": {
			On429:         []string{"claude-sonnet-4-6", "gpt-5.6-sol", "antigravity/gemini-3.8-flash-high"},
			OnUnavailable: []string{"gpt-5.6-sol", "gemini-3.7-flash", "k3"},
		},
		"claude-opus-5": {
			On429:         []string{"claude-sonnet-5", "gpt-6-astra", "gpt-5.6-sol"},
			OnUnavailable: []string{"gpt-6-astra", "claude-sonnet-5", "gemini-3.7-flash"},
		},
		"antigravity/gemini-3.8-flash-high": {
			On429:         []string{"antigravity/gemini-3.8-pro", "gemini-3.7-flash", "gpt-5.6-sol"},
			OnUnavailable: []string{"gemini-3.7-flash", "gpt-5.6-sol", "claude-sonnet-5"},
		},
		"antigravity/gemini-3.8-pro": {
			On429:         []string{"antigravity/gemini-3.8-flash-high", "gpt-5.6-sol", "claude-sonnet-5"},
			OnUnavailable: []string{"gpt-5.6-sol", "claude-sonnet-5", "gemini-3.7-flash"},
		},
		"gemini-3.7-flash": {
			On429:         []string{"gemini-3.5-flash", "gpt-5.6-luna", "claude-haiku-4-5"},
			OnUnavailable: []string{"gpt-5.6-luna", "claude-haiku-4-5", "antigravity/gemini-3.8-flash-high"},
		},
		"k3": {
			On429:         []string{"k3-256k", "gpt-5.6-sol", "claude-sonnet-5"},
			OnUnavailable: []string{"gpt-5.6-sol", "claude-sonnet-5", "gemini-3.7-flash"},
		},
		"MiniMax-M3": {
			On429:         []string{"MiniMax-M2.7", "gpt-5.6-sol", "claude-sonnet-5"},
			OnUnavailable: []string{"gpt-5.6-sol", "claude-sonnet-5", "gemini-3.7-flash"},
		},
		"glm-5.3": {
			On429:         []string{"glm-5.3-flash", "gpt-5.6-terra", "claude-sonnet-5"},
			OnUnavailable: []string{"gpt-5.6-terra", "claude-sonnet-5", "gemini-3.7-flash"},
		},
		"mimo-v2.5-pro": {
			On429:         []string{"mimo-v2.5", "gpt-5.6-sol", "claude-sonnet-5"},
			OnUnavailable: []string{"gpt-5.6-sol", "claude-sonnet-5", "gemini-3.7-flash"},
		},
		"grok-4.6": {
			On429:         []string{"grok-4.5", "gpt-5.6-sol", "claude-sonnet-5"},
			OnUnavailable: []string{"gpt-5.6-sol", "claude-sonnet-5", "gemini-3.7-flash"},
		},
	}
}

// SetRoute overrides or registers a fallback route for a model.
func (fg *FallbackGraph) SetRoute(model string, rule FallbackRule) {
	fg.mu.Lock()
	defer fg.mu.Unlock()
	fg.routes[strings.ToLower(strings.TrimSpace(model))] = rule
}

func (fg *FallbackGraph) AddCandidate(model, candidate string) {
	if fg == nil {
		return
	}
	model = strings.ToLower(strings.TrimSpace(model))
	candidate = strings.TrimSpace(candidate)
	if model == "" || candidate == "" {
		return
	}
	fg.mu.Lock()
	defer fg.mu.Unlock()
	rule := fg.routes[model]
	appendUnique := func(values []string) []string {
		for _, value := range values {
			if strings.EqualFold(value, candidate) {
				return values
			}
		}
		return append(values, candidate)
	}
	rule.On429 = appendUnique(rule.On429)
	rule.OnUnavailable = appendUnique(rule.OnUnavailable)
	fg.routes[model] = rule
}

// ResolveFallback finds a compatible alternative model according to the fallback graph
// and verifies that the candidate passes capability gates and circuit breaker checks.
func (fg *FallbackGraph) ResolveFallback(
	currentModel string,
	trigger FallbackTrigger,
	caps RequestCapabilities,
	pool *poolState,
	cb *CircuitBreakerManager,
) (string, string, bool) {
	return fg.ResolveFallbackWithTransition(currentModel, trigger, caps, pool, cb, nil)
}

func (fg *FallbackGraph) ResolveFallbackWithTransition(
	currentModel string,
	trigger FallbackTrigger,
	caps RequestCapabilities,
	pool *poolState,
	cb *CircuitBreakerManager,
	conversation *ConversationState,
) (string, string, bool) {
	return fg.ResolveFallbackWithTransitionExcluding(currentModel, trigger, caps, pool, cb, conversation, nil)
}

func (fg *FallbackGraph) ResolveFallbackWithTransitionExcluding(
	currentModel string,
	trigger FallbackTrigger,
	caps RequestCapabilities,
	pool *poolState,
	cb *CircuitBreakerManager,
	conversation *ConversationState,
	exclude map[string]bool,
) (string, string, bool) {
	if fg == nil {
		return "", "", false
	}

	fg.mu.RLock()
	defer fg.mu.RUnlock()

	normCurrent := strings.ToLower(strings.TrimSpace(currentModel))
	rule, exists := fg.routes[normCurrent]
	if !exists {
		// Try matching prefix / canonical without suffixes
		for prefix, r := range fg.routes {
			if strings.HasPrefix(normCurrent, prefix) {
				rule = r
				exists = true
				break
			}
		}
	}
	if !exists {
		// Generic default fallbacks across providers
		rule = FallbackRule{
			On429:         []string{"gpt-5.6-sol", "claude-sonnet-5", "antigravity/gemini-3.8-flash-high", "gemini-3.7-flash"},
			OnUnavailable: []string{"gpt-5.6-sol", "claude-sonnet-5", "gemini-3.7-flash", "k3"},
		}
	}

	var candidates []string
	switch trigger {
	case Trigger429:
		candidates = rule.On429
	case TriggerUnavailable:
		candidates = rule.OnUnavailable
	default:
		candidates = append(rule.On429, rule.OnUnavailable...)
	}

	bestCandidate := ""
	bestReason := ""
	bestCost := 0.0
	for index, cand := range candidates {
		if strings.EqualFold(cand, normCurrent) {
			continue
		}
		if exclude[strings.ToLower(strings.TrimSpace(cand))] {
			continue
		}

		// 1. Mandatory Compatibility Filter
		compatible, _ := isModelCompatible(cand, caps, pool)
		if !compatible {
			continue
		}

		// 2. Circuit Breaker Check
		meta, _ := lookupModelMetadata(cand, pool)
		plan := TransitionCompatibility{Allowed: true, Mode: TransitionNative}
		if conversation != nil {
			plan = CanTransition(*conversation, conversation.ActiveProvider, meta.Provider)
			if !plan.Allowed {
				continue
			}
		}
		if cb != nil {
			if allowed, _ := cb.AllowProvider(string(meta.Provider)); !allowed {
				continue
			}
		}

		// 3. Pool Account Availability Check
		if pool != nil {
			accounts := pool.accountsForType(meta.Provider)
			if len(accounts) == 0 {
				continue
			}
			hasLive := false
			now := time.Now()
			requiredPlan := requiredPlanForRequest(meta.Provider, nil, cand)
			for _, a := range accounts {
				a.mu.Lock()
				live := !a.Dead && !a.Disabled &&
					(a.RateLimitUntil.IsZero() || !a.RateLimitUntil.After(now)) &&
					planMatchesRequired(a.PlanType, requiredPlan)
				a.mu.Unlock()
				if live {
					hasLive = true
					break
				}
			}
			if !hasLive {
				continue
			}
		}

		// Candidate accepted
		fallbackReason := fmt.Sprintf("fallback_%s:%s->%s", trigger, currentModel, cand)
		if conversation == nil {
			return cand, fallbackReason, true
		}
		candidateCost := float64(index)*0.1 + plan.Cost
		if bestCandidate == "" || candidateCost < bestCost {
			bestCandidate, bestCost = cand, candidateCost
			bestReason = fallbackReason + ":" + string(plan.Mode)
		}
	}
	if bestCandidate != "" {
		return bestCandidate, bestReason, true
	}
	return "", "", false
}
