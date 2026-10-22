package main

import (
	"errors"
	"log"
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

// consumeAccountResetCredit spends one banked reset on an operator-managed
// Codex account. The request names no credit: the soonest-expiring one is
// chosen from the upstream's current list.
func (h *proxyHandler) consumeAccountResetCredit(w http.ResponseWriter, r *http.Request, id string) {
	var target *Account
	if h.pool != nil {
		for _, a := range h.pool.allAccounts() {
			if a.ID == id {
				target = a
				break
			}
		}
	}
	if target == nil {
		respondJSONError(w, http.StatusNotFound, "account not found")
		return
	}
	actor := providerContributionActor(r)
	if h.passport != nil {
		if err := h.passport.authorizeAccount(actor, target, "consume_reset"); err != nil {
			respondJSONError(w, http.StatusForbidden, "reset consumption denied")
			return
		}
		if err := h.passport.recordAudit(actor, "account.reset_credit_consume_requested", id, ""); err != nil {
			respondJSONError(w, http.StatusServiceUnavailable, "account audit unavailable")
			return
		}
	}

	result, err := h.consumeEarliestCodexResetCredit(target)
	switch {
	case errors.Is(err, errResetCreditUnavailable), errors.Is(err, errResetCreditInProgress):
		respondJSONError(w, http.StatusConflict, err.Error())
		return
	case err != nil:
		log.Printf("reset credit consume %s failed: %v", id, err)
		respondJSONError(w, http.StatusBadGateway, "unable to consume the banked reset")
		return
	}
	if h.passport != nil {
		if err := h.passport.recordAudit(actor, "account.reset_credit_consumed", id, result.CreditID); err != nil {
			log.Printf("reset credit consumed %s but audit failed: %v", id, err)
		}
	}
	noStore(w)
	respondJSON(w, result)
}
