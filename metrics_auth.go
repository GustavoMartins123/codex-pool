package main

import (
	"net/http"
	"strings"
)

func (h *proxyHandler) checkMetricsAuth(w http.ResponseWriter, r *http.Request) bool {
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
		return false
	}
	if authorization := r.Header.Get("Authorization"); authorization != "" {
		// The metrics bearer is a long-lived secret like the admin token, so it
		// shares the brute-force ban: refuse banned peers before comparing and
		// count every wrong guess.
		ip := getClientIP(r)
		if h.bruteForce != nil && h.bruteForce.isBanned(ip) {
			http.Error(w, "too many failed attempts, try again later", http.StatusTooManyRequests)
			return false
		}
		token, ok := strings.CutPrefix(authorization, "Bearer ")
		if !ok || h.cfg == nil || h.cfg.metricsToken == "" || !secureSecretEquals(token, h.cfg.metricsToken) {
			if h.bruteForce != nil {
				h.bruteForce.recordFailure(ip)
			}
			http.Error(w, "invalid metrics credential", http.StatusForbidden)
			return false
		}
		if h.bruteForce != nil {
			h.bruteForce.recordSuccess(ip)
		}
		return true
	}
	return h.checkAdminAuth(w, r)
}
