package main

import (
	"encoding/json"
	"errors"
	"go.etcd.io/bbolt"
	"io"
	"net/http"
)

type accountControlView struct {
	Provider AccountType     `json:"provider"`
	ID       string          `json:"id"`
	Revision uint64          `json:"revision"`
	Controls accountControls `json:"controls"`
	Inflight int             `json:"inflight"`
}

func decodeGovernanceJSON(w http.ResponseWriter, r *http.Request, dst any) error {
	d := json.NewDecoder(http.MaxBytesReader(w, r.Body, 32<<10))
	d.DisallowUnknownFields()
	if err := d.Decode(dst); err != nil {
		return err
	}
	var extra any
	if err := d.Decode(&extra); !errors.Is(err, io.EOF) {
		return errors.New("expected one JSON object")
	}
	return nil
}

func (h *proxyHandler) handleAccountControls(w http.ResponseWriter, r *http.Request, provider AccountType, id string) {
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
	if err := h.passport.authorizeAccount(actor.ID, &Account{Type: provider, ID: id}, "manage"); err != nil {
		respondJSONError(w, 403, "account management denied")
		return
	}
	if r.Method == http.MethodPut {
		if !h.passportCSRF(r, session) {
			respondJSONError(w, 403, "invalid CSRF token")
			return
		}
		var q struct {
			Revision uint64           `json:"revision"`
			Controls *accountControls `json:"controls"`
			Reason   string           `json:"reason"`
		}
		if decodeGovernanceJSON(w, r, &q) != nil || q.Controls == nil {
			respondJSONError(w, 400, "invalid controls request")
			return
		}
		if err := h.passport.updateAccountControls(actor.ID, provider, id, q.Revision, *q.Controls, q.Reason); err != nil {
			respondPolicyError(w, err)
			return
		}
	} else if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	p := h.passport
	p.accountMu.Lock()
	defer p.accountMu.Unlock()
	var view accountControlView
	if err := p.db.View(func(tx *bbolt.Tx) error {
		r, err := readAccountResource(tx.Bucket([]byte(bucketAccountResources)), provider, id)
		if err == nil {
			view = accountControlView{Provider: provider, ID: id, Revision: r.Revision, Controls: r.Controls, Inflight: p.accountInflight[resourceKey(provider, id)]}
		}
		return err
	}); err != nil {
		respondJSONError(w, 503, "account controls unavailable")
		return
	}
	respondJSON(w, view)
}
