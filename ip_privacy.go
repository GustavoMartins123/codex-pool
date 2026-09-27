package main

import (
	"fmt"
	"time"
)

// withHashWindow derives a per-window salt so origin hashes stay stable
// within a window (daily/weekly analytics keep joining) but cannot be
// correlated across windows. window <= 0 keeps the salt stable forever,
// preserving long-term anonymous attribution for existing deployments.
func withHashWindow(salt string, window time.Duration, now time.Time) string {
	if window <= 0 {
		return salt
	}
	epoch := now.UTC().Unix() / int64(window/time.Second)
	return fmt.Sprintf("%s|w%d", salt, epoch)
}

// traceClientIP returns the client identity for route traces: the raw IP
// unless privacy mode is on, in which case the same salted hash used for
// origin analytics keeps traces correlatable without persisting PII.
func (h *proxyHandler) traceClientIP(clientIP string) string {
	if h == nil || h.cfg == nil || !h.cfg.ipPrivacy {
		return clientIP
	}
	if clientIP == "" {
		return ""
	}
	return hashUserIP(clientIP, h.originHashSalt())
}

// rawIPForAdmin hides stored raw IPs from admin responses in privacy mode.
func (h *proxyHandler) rawIPForAdmin(rawIP string) string {
	if h != nil && h.cfg != nil && h.cfg.ipPrivacy {
		return ""
	}
	return rawIP
}
