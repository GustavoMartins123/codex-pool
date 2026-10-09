package main

import (
	"encoding/json"
	"errors"
	"go.etcd.io/bbolt"
	"net/http"
	"sort"
	"time"
)

type accountSharingView struct {
	Provider            AccountType    `json:"provider"`
	ID                  string         `json:"id"`
	OwnerID             string         `json:"owner_id"`
	Revision            uint64         `json:"revision"`
	OperatorMayDelegate bool           `json:"operator_may_delegate"`
	Owner               bool           `json:"owner"`
	Grants              []accountGrant `json:"grants"`
}

func (h *proxyHandler) handleAccountSharing(w http.ResponseWriter, r *http.Request, provider AccountType, id, action string) {
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
	if r.Method != http.MethodGet {
		if !h.passportCSRF(r, session) {
			respondJSONError(w, 403, "invalid CSRF token")
			return
		}
		var err error
		switch {
		case action == "delegation" && r.Method == http.MethodPut:
			var q struct {
				Revision uint64 `json:"revision"`
				Allowed  *bool  `json:"allowed"`
			}
			if decodeGovernanceJSON(w, r, &q) != nil || q.Allowed == nil {
				respondJSONError(w, 400, "invalid delegation request")
				return
			}
			err = p.setAccountDelegation(actor.ID, provider, id, q.Revision, *q.Allowed)
		case action == "grants" && (r.Method == http.MethodPost || r.Method == http.MethodPut):
			var q struct {
				Revision      uint64       `json:"revision"`
				GrantRevision uint64       `json:"grant_revision,omitempty"`
				ID            string       `json:"id"`
				RecipientID   string       `json:"recipient_id"`
				Models        []string     `json:"models"`
				Budget        PolicyLimits `json:"budget"`
				ExpiresAt     time.Time    `json:"expires_at"`
				Reason        string       `json:"reason"`
			}
			if decodeGovernanceJSON(w, r, &q) != nil {
				respondJSONError(w, 400, "invalid grant request")
				return
			}
			draft := accountGrant{ID: q.ID, Provider: provider, AccountID: id, RecipientID: q.RecipientID, Models: q.Models, Budget: q.Budget, ExpiresAt: q.ExpiresAt, Reason: q.Reason}
			if r.Method == http.MethodPut {
				_, err = p.updateAccountGrant(actor.ID, q.Revision, q.GrantRevision, draft)
			} else {
				_, err = p.createAccountGrant(actor.ID, q.Revision, draft)
			}
		default:
			http.Error(w, "method not allowed", 405)
			return
		}
		if err != nil {
			respondPolicyError(w, err)
			return
		}
	}
	var resource *accountResource
	var grants []accountGrant
	if err := p.db.View(func(tx *bbolt.Tx) error {
		var err error
		resource, err = readAccountResource(tx.Bucket([]byte(bucketAccountResources)), provider, id)
		if err != nil {
			return err
		}
		grants, err = accountGrantsByResource(tx, provider, id, "")
		return err
	}); err != nil {
		respondJSONError(w, 503, "sharing unavailable")
		return
	}
	if !p.canDelegate(actor.ID, resource) && actor.ID != resource.OwnerID {
		respondJSONError(w, 403, "account delegation denied")
		return
	}
	visible := []accountGrant{}
	for _, g := range grants {
		if g.Provider == provider && g.AccountID == id {
			visible = append(visible, g)
		}
	}
	respondJSON(w, accountSharingView{Provider: provider, ID: id, OwnerID: resource.OwnerID, Revision: resource.Revision, OperatorMayDelegate: resource.OperatorMayDelegate, Owner: actor.ID == resource.OwnerID, Grants: visible})
}

func (h *proxyHandler) handleGrantRevoke(w http.ResponseWriter, r *http.Request, id string) {
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
	if r.Method != http.MethodDelete {
		http.Error(w, "method not allowed", 405)
		return
	}
	if !h.passportCSRF(r, session) {
		respondJSONError(w, 403, "invalid CSRF token")
		return
	}
	var q struct {
		Revision uint64 `json:"revision"`
	}
	if decodeGovernanceJSON(w, r, &q) != nil {
		respondJSONError(w, 400, "invalid revoke request")
		return
	}
	if err := h.passport.revokeAccountGrant(actor.ID, id, q.Revision); err != nil {
		respondPolicyError(w, err)
		return
	}
	respondJSON(w, map[string]any{"id": id, "revoked": true})
}

func (h *proxyHandler) handleShareableAccounts(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	actor, _, ok := h.requireOperator(w, r)
	if !ok {
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	resources := []accountResource{}
	if err := h.passport.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(bucketAccountResources))
		if b == nil {
			return errors.New("account authority unavailable")
		}
		return b.ForEach(func(_, v []byte) error {
			var r accountResource
			if err := json.Unmarshal(v, &r); err != nil {
				return err
			}
			canonical, err := readAccountResource(b, r.Provider, r.ID)
			if err != nil {
				return err
			}
			resources = append(resources, *canonical)
			return nil
		})
	}); err != nil {
		respondJSONError(w, 503, "account resources unavailable")
		return
	}
	views := []map[string]any{}
	for _, r := range resources {
		if r.Status == "active" && r.WithdrawnAt == nil && h.passport.canDelegate(actor.ID, &r) {
			views = append(views, map[string]any{"id": r.ID, "provider": r.Provider, "owner_id": r.OwnerID})
		}
	}
	sort.Slice(views, func(i, j int) bool { return views[i]["id"].(string) < views[j]["id"].(string) })
	respondJSON(w, views)
}
