package main

import (
	"math"
	"net/http"
	"time"
)

const memberQuotaMaxAge = 15 * time.Minute

// Only this projection may cross the member boundary. Account identifiers,
// credentials, ownership, upstream errors and raw provider data stay private.
type memberQuotaWindow struct {
	RemainingPercent float64   `json:"remaining_percent"`
	ReportedAccounts int       `json:"reported_accounts"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type memberModelQuota struct {
	Model              string             `json:"model"`
	Name               string             `json:"name"`
	Provider           string             `json:"provider"`
	SupportingAccounts int                `json:"supporting_accounts"`
	AvailableNow       bool               `json:"available_now"`
	Primary            *memberQuotaWindow `json:"primary"`
	Secondary          *memberQuotaWindow `json:"secondary"`
}

func addMemberQuotaWindow(window **memberQuotaWindow, used float64, reported bool, updated, reset, now time.Time) {
	if !reported || updated.IsZero() || updated.After(now) || now.Sub(updated) > memberQuotaMaxAge ||
		!reset.IsZero() && !reset.After(now) || math.IsNaN(used) || math.IsInf(used, 0) || used < 0 || used > 1 {
		return
	}
	if *window == nil {
		*window = &memberQuotaWindow{UpdatedAt: updated}
	}
	w := *window
	w.RemainingPercent += (1 - used) * 100
	w.ReportedAccounts++
	if updated.Before(w.UpdatedAt) {
		w.UpdatedAt = updated
	}
}

func (h *proxyHandler) handleMyQuotas(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodGet {
		w.Header().Set("Allow", http.MethodGet)
		respondJSONError(w, http.StatusMethodNotAllowed, "method not allowed")
		return
	}
	if h.passport == nil {
		respondJSONError(w, 503, "account authority unavailable")
		return
	}
	principal, _ := h.passport.authenticate(r)
	if principal == nil {
		respondJSONError(w, 401, "authentication required")
		return
	}
	if principal.Kind != PrincipalMember && principal.Kind != PrincipalOperator {
		respondJSONError(w, 403, "member access required")
		return
	}
	if h.pool == nil || h.pool.accountAuthority != h.passport || h.cfg == nil {
		respondJSONError(w, 503, "account authority unavailable")
		return
	}
	// Evaluate before catalog filtering: broken policy configuration must be an
	// explicit error, not a successful empty catalog.
	if _, _, _, err := h.passport.evaluateClientPolicy(principal.ID, "", h.cfg.hotClientPolicies()); err != nil {
		respondJSONError(w, 503, "policy unavailable")
		return
	}
	visible := h.catalogPool(principal.ID)
	rows := []memberModelQuota{}
	now := time.Now().UTC()
	for _, model := range poolModelDescriptors(visible) {
		if model.SupportingAccounts == 0 {
			continue
		}
		row := memberModelQuota{Model: model.ID, Name: model.Name, Provider: model.Provider,
			SupportingAccounts: model.SupportingAccounts}
		for _, account := range visible.allAccounts() {
			if string(account.Type) != model.Provider || !visible.catalogAccountAllows(account, model.ID) {
				continue
			}
			definition, _ := modelForProvider(account.Type, model.ID)
			usable := h.passport.authorizeAccount(principal.ID, account, "use") == nil
			account.mu.Lock()
			if definition.RequiresDiscovery {
				if _, ok := accountDiscoveredModel(account, model.ID); !ok {
					account.mu.Unlock()
					continue
				}
			}
			u := account.Usage
			row.AvailableNow = row.AvailableNow || usable && accountAvailableForRoutingLocked(account, now)
			addMemberQuotaWindow(&row.Primary, u.PrimaryUsedPercent, u.PrimaryUsageReported && u.primarySet, u.RetrievedAt, u.PrimaryResetAt, now)
			addMemberQuotaWindow(&row.Secondary, u.SecondaryUsedPercent, u.SecondaryUsageReported && u.secondarySet, u.RetrievedAt, u.SecondaryResetAt, now)
			account.mu.Unlock()
		}
		for _, window := range []*memberQuotaWindow{row.Primary, row.Secondary} {
			if window != nil {
				window.RemainingPercent /= float64(window.ReportedAccounts)
			}
		}
		rows = append(rows, row)
	}
	respondJSON(w, map[string]any{"models": rows, "generated_at": now})
}
