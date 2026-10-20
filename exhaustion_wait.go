package main

import (
	"context"
	"log"
	"net/http"
	"strconv"
	"time"
)

// exhaustionWaitPollInterval is how often a held request re-checks pool
// routability while waiting for a usage window to reset.
const exhaustionWaitPollInterval = 2 * time.Second

// capacityWaitBudget clamps the configured hold to the request deadline.
func (h *proxyHandler) capacityWaitBudget(ctx context.Context) time.Duration {
	budget := h.cfg.exhaustionWait
	if d, ok := ctx.Deadline(); ok {
		if remaining := time.Until(d); remaining < budget {
			budget = remaining
		}
	}
	return budget
}

func (p *poolState) capacityWaitExclusions(userID, conversationID string, excluded map[string]bool, accountType AccountType, requiredPlan, clientIP, model string) map[string]bool {
	excluded = p.accountExclusions(userID, excluded, model)
	excluded = p.controlExclusions(userID, conversationID, excluded)
	p.mu.RLock()
	defer p.mu.RUnlock()
	requiresDiscovery := p.discoveredModelRequiresEntitlementLocked(accountType, model)
	for _, account := range p.accounts {
		account.mu.Lock()
		eligible := account.Type == accountType && planMatchesRequired(account.PlanType, requiredPlan) && accountAllowsClientIPLocked(account, clientIP)
		if eligible && requiresDiscovery {
			_, eligible = accountDiscoveredModel(account, model)
		}
		if !eligible {
			excluded[account.ID] = true
		}
		account.mu.Unlock()
	}
	return excluded
}

func (h *proxyHandler) usageResetWithinRequestBudget(ctx context.Context, accountType AccountType, exclude map[string]bool) bool {
	return h.pool.usageResetWithinBudget(accountType, h.capacityWaitBudget(ctx), exclude)
}

func respondCapacityError(w http.ResponseWriter, code string, retryIn time.Duration) {
	if retryIn > 0 {
		seconds := int64((retryIn + time.Second - 1) / time.Second)
		w.Header().Set("Retry-After", strconv.FormatInt(seconds, 10))
	}
	respondPolicyError(w, &policyError{Status: http.StatusTooManyRequests, Code: code,
		Message: "The requested provider has no available account capacity within the request wait budget."})
}

// waitForUsageReset holds a request while every account of the type is
// usage-exhausted (5h/weekly window at the hard-exclude thresholds) and at
// least one of them has a known future window reset. It returns true once an
// account is routable again — typically after the window reset passed and the
// periodic usage poll observed it — so the caller can retry immediately. It
// returns false when there is nothing to wait for, the bounded wait budget
// expires, or the client goes away.
func (h *proxyHandler) waitForUsageReset(ctx context.Context, accountType AccountType, reqID string, exclude map[string]bool) bool {
	if ctx.Err() != nil {
		return false
	}
	if h.pool.hasRoutableAccountOfType(accountType, exclude) {
		return true
	}
	nearest, known := h.pool.nearestUsageReset(accountType, exclude)
	if !known || nearest > h.capacityWaitBudget(ctx) {
		return false
	}

	deadline := time.Now().Add(h.cfg.exhaustionWait)
	if d, ok := ctx.Deadline(); ok && d.Before(deadline) {
		deadline = d
	}
	tick := exhaustionWaitPollInterval
	if shorter := h.cfg.exhaustionWait / 8; shorter > 0 && shorter < tick {
		tick = shorter
	}
	log.Printf("[%s] all %s accounts usage-exhausted (nearest window reset in %s); holding request for up to %s",
		reqID, accountType, nearest.Round(time.Second), time.Until(deadline).Round(time.Second))

	for {
		wait := time.Until(deadline)
		if wait <= 0 {
			log.Printf("[%s] usage-exhaustion hold expired for %s accounts; giving up", reqID, accountType)
			return false
		}
		if tick < wait {
			wait = tick
		}
		select {
		case <-time.After(wait):
		case <-ctx.Done():
			return false
		}
		if h.pool.hasRoutableAccountOfType(accountType, exclude) {
			log.Printf("[%s] %s account available again after usage window reset", reqID, accountType)
			return true
		}
		if _, known := h.pool.nearestUsageReset(accountType, exclude); !known {
			return false
		}
	}
}
