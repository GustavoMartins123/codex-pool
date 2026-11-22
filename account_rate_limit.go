package main

import (
	"log"
	"net/http"
	"strconv"
	"strings"
	"time"

	"codex-pool-proxy/internal/ratelimit"
)

func (h *proxyHandler) logRateLimitResponseHeaders(reqID string, accountType AccountType, hdr http.Header) {
	if h == nil || !h.cfg.debug.Load() {
		return
	}
	if hdr == nil {
		return
	}

	keys := []string{
		"anthropic-ratelimit-unified-primary-utilization",
		"anthropic-ratelimit-unified-secondary-utilization",
		"anthropic-ratelimit-unified-tokens-utilization",
		"anthropic-ratelimit-unified-requests-utilization",
		"anthropic-ratelimit-unified-5h-utilization",
		"anthropic-ratelimit-unified-7d-utilization",
		"anthropic-ratelimit-unified-reset",
		"anthropic-ratelimit-unified-primary-reset",
		"anthropic-ratelimit-unified-secondary-reset",
		"anthropic-ratelimit-unified-tokens-reset",
		"anthropic-ratelimit-unified-requests-reset",
		"anthropic-ratelimit-unified-5h-reset",
		"anthropic-ratelimit-unified-7d-reset",
		"anthropic-ratelimit-unified-status",
		"anthropic-ratelimit-unified-5h-status",
		"anthropic-ratelimit-unified-7d-status",
		"x-ratelimit-limit-requests",
		"x-ratelimit-remaining-requests",
		"x-ratelimit-reset-requests",
		"x-ratelimit-limit-tokens",
		"x-ratelimit-remaining-tokens",
		"x-ratelimit-reset-tokens",
		"x-ratelimit-limit",
		"x-ratelimit-remaining",
		"x-ratelimit-reset",
	}

	parts := make([]string, 0, len(keys))
	for _, key := range keys {
		if v := strings.TrimSpace(hdr.Get(key)); v != "" {
			parts = append(parts, key+"="+v)
		}
	}

	if len(parts) == 0 {
		for key := range hdr {
			lower := strings.ToLower(key)
			if strings.Contains(lower, "ratelimit") || strings.Contains(lower, "rate-limit") || strings.Contains(lower, "anthropic-ratelimit") {
				parts = append(parts, key+"="+hdr.Get(key))
			}
		}
	}

	if len(parts) == 0 {
		return
	}
	log.Printf("[%s] upstream %s rate-limit headers: %s", reqID, accountType, strings.Join(parts, ", "))
}

func parseRetryAfter(h http.Header) (time.Duration, bool) {
	if h == nil {
		return 0, false
	}
	val := strings.TrimSpace(h.Get("Retry-After"))
	if val == "" {
		return 0, false
	}
	if secs, err := strconv.ParseInt(val, 10, 64); err == nil {
		if secs <= 0 {
			return 0, false
		}
		return time.Duration(secs) * time.Second, true
	}
	if when, err := http.ParseTime(val); err == nil {
		wait := time.Until(when)
		if wait <= 0 {
			return 0, false
		}
		return wait, true
	}
	return 0, false
}

// backoffDuration returns the exponential backoff for the given level.
// Formula: min(1s * 2^level, 30m). Level 0 = 1s, 1 = 2s, 2 = 4s, ... 10 = ~17m, 11+ = 30m.
func backoffDuration(level int) time.Duration {
	const base = 1 * time.Second
	const maxBackoff = 30 * time.Minute
	d := base << uint(level) // 1s * 2^level
	if d > maxBackoff || d <= 0 {
		return maxBackoff
	}
	return d
}

func (h *proxyHandler) applyRateLimit(a *Account, hdr http.Header) time.Duration {
	if a == nil {
		return 0
	}

	// Try multiple sources for the cooldown duration, in priority order:
	// 1. Provider reset timestamp headers
	// 2. Retry-After header (standard HTTP)
	// 3. Exponential backoff (fallback)
	wait := time.Duration(0)
	gotPreciseReset := false

	if hdr != nil {
		// Prefer the latest explicit reset. A 429 can exhaust more than one
		// request or token window, and retrying at the earlier boundary just
		// produces another 429.
		var preciseReset time.Time
		for _, key := range []string{
			"anthropic-ratelimit-unified-reset",
			"anthropic-ratelimit-unified-primary-reset",
			"anthropic-ratelimit-unified-5h-reset",
			"x-ratelimit-reset-requests",
			"x-ratelimit-reset-tokens",
			"x-ratelimit-reset",
		} {
			if resetStr := hdr.Get(key); resetStr != "" {
				if resetAt, ok := ratelimit.ParseReset(resetStr); ok && resetAt.After(time.Now()) && resetAt.After(preciseReset) {
					preciseReset = resetAt
				}
			}
		}
		if !preciseReset.IsZero() {
			wait = time.Until(preciseReset)
			gotPreciseReset = true
		}
	}

	if !gotPreciseReset {
		if w, ok := parseRetryAfter(hdr); ok {
			wait = w
		} else {
			a.mu.Lock()
			wait = backoffDuration(a.BackoffLevel)
			a.mu.Unlock()
		}
	}

	// Always bump backoff level so repeated rate limits get longer fallbacks.
	a.mu.Lock()
	a.BackoffLevel++
	a.mu.Unlock()

	// Also parse full rate limit utilization from the 429 response headers.
	// This updates the account's usage snapshot so candidate selection
	// can factor in the current utilization level.
	if hdr != nil {
		snap, ok := parseClaudeResponseRateLimits(hdr)
		if ok {
			a.mu.Lock()
			a.Usage = mergeUsage(a.Usage, snap)
			a.mu.Unlock()
		}
	}

	until := time.Now().Add(wait)
	if wait <= 0 {
		return 0
	}

	a.mu.Lock()
	if a.RateLimitUntil.Before(until) {
		a.RateLimitUntil = until
	}
	a.mu.Unlock()
	if h.cfg.debug.Load() {
		log.Printf("rate-limit backoff: account=%s level=%d wait=%s precise=%v", a.ID, a.BackoffLevel, wait, gotPreciseReset)
	}
	return wait
}
