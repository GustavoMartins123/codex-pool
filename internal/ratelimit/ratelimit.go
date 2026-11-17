// Package ratelimit parses the rate-limit utilization and reset signals that
// providers return in response headers.
package ratelimit

import (
	"net/http"
	"strconv"
	"strings"
	"time"
)

// ParseFloat reads a numeric header value, tolerating a trailing "%" suffix.
func ParseFloat(value string) (float64, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0, false
	}
	if strings.HasSuffix(value, "%") {
		value = strings.TrimSpace(strings.TrimSuffix(value, "%"))
	}
	f, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return 0, false
	}
	return f, true
}

// ClampPercent bounds a used ratio to the [0, 1] interval.
func ClampPercent(value float64) float64 {
	if value < 0 {
		return 0
	}
	if value > 1 {
		return 1
	}
	return value
}

// ParsePercent accepts values in either percent (0-100) or ratio (0-1) form.
func ParsePercent(value string) (float64, bool) {
	raw, ok := ParseFloat(value)
	if !ok || raw < 0 {
		return 0, false
	}
	if raw > 1 {
		raw = raw / 100.0
	}
	return ClampPercent(raw), true
}

// UsageFromRemainingLimit derives the used ratio from a remaining/limit header
// pair.
func UsageFromRemainingLimit(headers http.Header, remainingKey, limitKey string) (float64, bool) {
	remaining, ok := ParseFloat(headers.Get(remainingKey))
	if !ok {
		return 0, false
	}
	limit, ok := ParseFloat(headers.Get(limitKey))
	if !ok || limit <= 0 {
		return 0, false
	}
	return ClampPercent((limit - remaining) / limit), true
}

// ParseReset reads a reset instant expressed as RFC3339 or as a Unix timestamp
// in seconds (milliseconds when the value is clearly in that domain).
func ParseReset(value string) (time.Time, bool) {
	value = strings.TrimSpace(value)
	if value == "" {
		return time.Time{}, false
	}
	if parsed, err := time.Parse(time.RFC3339, value); err == nil {
		return parsed, true
	}
	if parsed, err := strconv.ParseFloat(value, 64); err == nil && parsed > 0 {
		sec := int64(parsed)
		// Handle ms timestamps if they are clearly in that domain.
		if sec > 1e12 {
			sec = int64(parsed / 1000)
		}
		return time.Unix(sec, 0), true
	}
	return time.Time{}, false
}
