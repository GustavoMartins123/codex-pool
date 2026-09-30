package main

import (
	"encoding/json"
	"errors"
	"go.etcd.io/bbolt"
	"net/http"
	"sort"
	"strings"
	"time"
)

type myAccountView struct {
	ID          string      `json:"id"`
	Provider    AccountType `json:"provider"`
	Status      string      `json:"status"`
	State       string      `json:"state"`
	Revision    uint64      `json:"revision"`
	WithdrawnAt *time.Time  `json:"withdrawn_at,omitempty"`
}

func (p *PassportStore) ownedAccountResources(actor string) ([]accountResource, error) {
	result := []accountResource{}
	err := p.db.View(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(bucketAccountResources))
		if b == nil {
			return errors.New("account authority unavailable")
		}
		return b.ForEach(func(k, v []byte) error {
			var r accountResource
			if err := json.Unmarshal(v, &r); err != nil {
				return err
			}
			if _, err := readAccountResource(b, r.Provider, r.ID); err != nil {
				return err
			}
			if r.OwnerID == actor && !r.OperatorManaged {
				result = append(result, r)
			}
			return nil
		})
	})
	sort.Slice(result, func(i, j int) bool {
		if result[i].Provider == result[j].Provider {
			return result[i].ID < result[j].ID
		}
		return result[i].Provider < result[j].Provider
	})
	return result, err
}

func (h *proxyHandler) handleMyAccounts(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if h.passport == nil {
		respondJSONError(w, 503, "account authority unavailable")
		return
	}
	principal, _ := h.passport.authenticate(r)
	if principal == nil {
		respondJSONError(w, 401, "authentication required")
		return
	}
	if r.Method != http.MethodGet {
		http.Error(w, "method not allowed", 405)
		return
	}
	resources, err := h.passport.ownedAccountResources(principal.ID)
	if err != nil {
		respondJSONError(w, 503, "could not load accounts")
		return
	}
	accounts := map[string]*Account{}
	if h.pool != nil {
		for _, a := range h.pool.allAccounts() {
			accounts[resourceKey(a.Type, a.ID)] = a
		}
	}
	views := []myAccountView{}
	for _, resource := range resources {
		view := myAccountView{ID: resource.ID, Provider: resource.Provider, Revision: resource.Revision, Status: resource.Status, State: resource.Status, WithdrawnAt: resource.WithdrawnAt}
		if resource.WithdrawnAt != nil {
			view.Status = "withdrawn"
			view.State = "withdrawn"
		} else if resource.Status == "active" {
			account := accounts[resourceKey(resource.Provider, resource.ID)]
			if account == nil {
				view.State = "unavailable"
			} else {
				account.mu.Lock()
				view.State = strings.ToLower(string(accountLifecycleStateLocked(account, time.Now())))
				account.mu.Unlock()
			}
		}
		views = append(views, view)
	}
	respondJSON(w, views)
}

func (p *PassportStore) withdrawAccount(actor string, provider AccountType, id string, revision uint64) error {
	p.contributionMu.Lock()
	defer p.contributionMu.Unlock()
	return p.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(bucketAccountResources))
		resource, err := readAccountResource(b, provider, id)
		if err != nil {
			return err
		}
		if resource.OwnerID != actor || resource.OperatorManaged {
			return &policyError{Status: 403, Code: "account_access_denied", Message: "account withdrawal denied"}
		}
		if resource.WithdrawnAt != nil {
			return nil
		}
		if revision == 0 || revision != resource.Revision {
			return &policyError{Status: 409, Code: "account_revision_conflict", Message: "account changed; reload before withdrawing"}
		}
		now := time.Now().UTC()
		resource.WithdrawnAt = &now
		resource.Revision++
		if err := putJSON(b, resourceKey(provider, id), resource); err != nil {
			return err
		}
		return p.audit(tx, actor, "account.withdrawn", id, string(provider))
	})
}

func (h *proxyHandler) handleMyAccountItem(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if h.passport == nil {
		respondJSONError(w, 503, "account authority unavailable")
		return
	}
	principal, session := h.passport.authenticate(r)
	if principal == nil {
		respondJSONError(w, 401, "authentication required")
		return
	}
	parts := strings.Split(strings.TrimPrefix(r.URL.Path, "/api/me/accounts/"), "/")
	if len(parts) != 3 || parts[2] != "withdraw" || r.Method != http.MethodPost {
		http.NotFound(w, r)
		return
	}
	if !h.passportCSRF(r, session) {
		respondJSONError(w, 403, "invalid CSRF token")
		return
	}
	var q struct {
		Revision uint64 `json:"revision"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&q) != nil {
		respondJSONError(w, 400, "invalid json")
		return
	}
	if err := h.passport.withdrawAccount(principal.ID, AccountType(parts[0]), parts[1], q.Revision); err != nil {
		respondPolicyError(w, err)
		return
	}
	if h.pool != nil {
		h.pool.mu.Lock()
		for conversation, id := range h.pool.convPin {
			if id == parts[1] {
				h.pool.unpinLocked(conversation)
			}
		}
		h.pool.mu.Unlock()
	}
	respondJSON(w, map[string]any{"id": parts[1], "status": "withdrawn"})
}
