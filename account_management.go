package main

import (
	"net/http"
	"strings"
)

func (h *proxyHandler) authorizeAccountManagement(w http.ResponseWriter, r *http.Request, id string) bool {
	if h.passport == nil {
		return true
	}
	actor := providerContributionActor(r)
	for _, a := range h.pool.allAccounts() {
		if a.ID != id {
			continue
		}
		if err := h.passport.authorizeAccount(actor, a, "manage"); err != nil {
			respondJSONError(w, http.StatusForbidden, "account management denied")
			return false
		}
		if err := h.passport.recordAudit(actor, "account.management_requested", id, r.URL.Path[strings.LastIndex(r.URL.Path, "/")+1:]); err != nil {
			respondJSONError(w, http.StatusServiceUnavailable, "account audit unavailable")
			return false
		}
		return true
	}
	respondJSONError(w, http.StatusNotFound, "account not found")
	return false
}
