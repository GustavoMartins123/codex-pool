package main

import (
	"encoding/json"
	"net/http"
	"sort"
	"time"

	"go.etcd.io/bbolt"
)

// Discovery exposes only the metadata needed to choose an account.
type operatorAccountView struct {
	Provider  AccountType `json:"provider"`
	ID        string      `json:"id"`
	OwnerID   string      `json:"owner_id"`
	Revision  uint64      `json:"revision"`
	Enabled   bool        `json:"enabled"`
	UpdatedAt *time.Time  `json:"updated_at,omitempty"`
	Reason    string      `json:"reason,omitempty"`
}

func operatorAccountViewFor(r *accountResource, operator string) operatorAccountView {
	v := operatorAccountView{Provider: r.Provider, ID: r.ID, OwnerID: r.OwnerID, Revision: r.Revision}
	if access, ok := r.OperatorAccess[operator]; ok {
		v.Enabled, v.Reason = access.Enabled, access.Reason
		v.UpdatedAt = &access.UpdatedAt
	}
	return v
}

func (h *proxyHandler) handleOperatorAccounts(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if h.passport == nil {
		respondJSONError(w, 503, "account authority unavailable")
		return
	}
	actor, _, ok := h.requireOperator(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	owner := r.URL.Query().Get("owner_id")
	if owner == "" {
		respondJSONError(w, 400, "owner_id is required")
		return
	}
	views := []operatorAccountView{}
	if err := h.passport.db.View(func(tx *bbolt.Tx) error {
		pr, err := accountPrincipal(tx, actor.ID)
		if err != nil || !principalIsUsableOperator(pr) {
			return accountControlError("operator_access_denied", 403)
		}
		b := tx.Bucket([]byte(bucketAccountResources))
		if b == nil {
			return accountControlError("account_authority_unavailable", 503)
		}
		return b.ForEach(func(_, raw []byte) error {
			var key accountResource
			if err := json.Unmarshal(raw, &key); err != nil {
				return err
			}
			resource, err := readAccountResource(b, key.Provider, key.ID)
			if err != nil {
				return err
			}
			if resource.OwnerID == owner && resource.OwnerID != actor.ID && !resource.OperatorManaged && resource.Status == "active" && resource.WithdrawnAt == nil {
				views = append(views, operatorAccountViewFor(resource, actor.ID))
			}
			return nil
		})
	}); err != nil {
		respondPolicyError(w, err)
		return
	}
	sort.Slice(views, func(i, j int) bool {
		return resourceKey(views[i].Provider, views[i].ID) < resourceKey(views[j].Provider, views[j].ID)
	})
	respondJSON(w, views)
}

func (h *proxyHandler) handleOperatorAccountAccess(w http.ResponseWriter, r *http.Request, provider AccountType, id string) {
	noStore(w)
	if h.passport == nil {
		respondJSONError(w, 503, "account authority unavailable")
		return
	}
	actor, session := h.passport.authenticate(r)
	if actor == nil {
		respondJSONError(w, 401, "authentication required")
		return
	}
	p := h.passport
	if r.Method == http.MethodPut {
		if actor.Kind != PrincipalOperator {
			respondJSONError(w, 403, "operator access required")
			return
		}
		if !h.passportCSRF(r, session) {
			respondJSONError(w, 403, "invalid CSRF token")
			return
		}
		var q struct {
			Revision uint64 `json:"revision"`
			Enabled  *bool  `json:"enabled"`
			Reason   string `json:"reason"`
		}
		if decodeGovernanceJSON(w, r, &q) != nil || q.Enabled == nil {
			respondJSONError(w, 400, "invalid operator access request")
			return
		}
		if err := p.setOperatorAccountAccess(actor.ID, provider, id, q.Revision, *q.Enabled, q.Reason); err != nil {
			respondPolicyError(w, err)
			return
		}
		var view operatorAccountView
		if err := p.db.View(func(tx *bbolt.Tx) error {
			resource, err := readAccountResource(tx.Bucket([]byte(bucketAccountResources)), provider, id)
			if err != nil {
				return err
			}
			view = operatorAccountViewFor(resource, actor.ID)
			return nil
		}); err != nil {
			respondPolicyError(w, err)
			return
		}
		respondJSON(w, view)
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	type accessView struct {
		OperatorID string `json:"operator_id"`
		operatorAccountAccess
	}
	views := []accessView{}
	if err := p.db.View(func(tx *bbolt.Tx) error {
		resource, err := readAccountResource(tx.Bucket([]byte(bucketAccountResources)), provider, id)
		if err != nil {
			return err
		}
		if resource.OwnerID != actor.ID {
			return accountControlError("account_access_denied", 403)
		}
		for operator, access := range resource.OperatorAccess {
			views = append(views, accessView{OperatorID: operator, operatorAccountAccess: access})
		}
		return nil
	}); err != nil {
		respondPolicyError(w, err)
		return
	}
	sort.Slice(views, func(i, j int) bool { return views[i].OperatorID < views[j].OperatorID })
	respondJSON(w, views)
}
