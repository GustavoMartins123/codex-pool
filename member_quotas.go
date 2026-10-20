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
	Compatibility      *modelVerification `json:"compatibility,omitempty"`
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
	now := time.Now().UTC()
	visible, err := h.memberQuotaSnapshot(principal.ID, now)
	if err != nil {
		respondJSONError(w, 503, "quota authorization unavailable")
		return
	}
	rows := []memberModelQuota{}
	accounts := visible.allAccounts()
	for _, model := range poolModelDescriptors(visible) {
		if model.SupportingAccounts == 0 {
			continue
		}
		row := memberModelQuota{Model: model.ID, Name: model.Name, Provider: model.Provider, Compatibility: model.Compatibility}
		for _, account := range accounts {
			if string(account.Type) != model.Provider || !visible.catalogAccountAllows(account, model.ID) {
				continue
			}
			definition, known := modelForProvider(account.Type, model.ID)
			_, grokKnown := grokModelByName(model.ID)
			if definition.RequiresDiscovery || !known && !(account.Type == AccountTypeGrok && grokKnown) && account.Type != AccountTypeAntigravity {
				if _, ok := accountDiscoveredModel(account, model.ID); !ok {
					continue
				}
			}
			row.SupportingAccounts++
			u := account.Usage
			row.AvailableNow = row.AvailableNow || accountAvailableForRoutingLocked(account, now)
			addMemberQuotaWindow(&row.Primary, u.PrimaryUsedPercent, u.PrimaryUsageReported && u.primarySet, u.RetrievedAt, u.PrimaryResetAt, now)
			addMemberQuotaWindow(&row.Secondary, u.SecondaryUsedPercent, u.SecondaryUsageReported && u.secondarySet, u.RetrievedAt, u.SecondaryResetAt, now)
		}
		if row.SupportingAccounts == 0 {
			continue
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
