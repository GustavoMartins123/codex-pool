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
		token, ok := strings.CutPrefix(authorization, "Bearer ")
		if !ok || h.cfg == nil || h.cfg.metricsToken == "" || !secureSecretEquals(token, h.cfg.metricsToken) {
			http.Error(w, "invalid metrics credential", http.StatusForbidden)
			return false
		}
		return true
	}
	return h.checkAdminAuth(w, r)
}
