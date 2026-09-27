package main

import (
	"context"
	"log"
	"time"
)

// exhaustionWaitPollInterval is how often a held request re-checks pool
// routability while waiting for a usage window to reset.
const exhaustionWaitPollInterval = 2 * time.Second

// usageResetWithinRequestBudget reports whether the provider's usage window
// resets within the effective hold budget: the configured exhaustion wait
// clamped by the request deadline.
func (h *proxyHandler) usageResetWithinRequestBudget(ctx context.Context, accountType AccountType) bool {
	budget := h.cfg.exhaustionWait
	if d, ok := ctx.Deadline(); ok {
		if remaining := time.Until(d); remaining < budget {
			budget = remaining
		}
	}
	return h.pool.usageResetWithinBudget(accountType, budget)
}

// waitForUsageReset holds a request while every account of the type is
// usage-exhausted (5h/weekly window at the hard-exclude thresholds) and at
// least one of them has a known future window reset. It returns true once an
// account is routable again — typically after the window reset passed and the
// periodic usage poll observed it — so the caller can retry immediately. It
// returns false when there is nothing to wait for, the bounded wait budget
// expires, or the client goes away.
func (h *proxyHandler) waitForUsageReset(ctx context.Context, accountType AccountType, reqID string) bool {
	if h.pool.hasRoutableAccountOfType(accountType) {
		return true
	}
	nearest, known := h.pool.nearestUsageReset(accountType, nil)
	if !known {
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
		if h.pool.hasRoutableAccountOfType(accountType) {
			log.Printf("[%s] %s account available again after usage window reset", reqID, accountType)
			return true
		}
		if _, known := h.pool.nearestUsageReset(accountType, nil); !known {
			return false
		}
	}
}
