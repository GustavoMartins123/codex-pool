package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"time"

	"go.etcd.io/bbolt"
)

const bucketAccountResources = "account_resources"

func migrateAccountResources(tx *bbolt.Tx) error {
	b := tx.Bucket([]byte(bucketAccountResources))
	var updates []accountResource
	if err := b.ForEach(func(k, v []byte) error {
		var r accountResource
		if err := json.Unmarshal(v, &r); err != nil {
			return err
		}
		if r.Version == 1 {
			r.Version = 2
			r.Status = "active"
			r.Revision++
			updates = append(updates, r)
		} else if r.Version != 2 {
			return errors.New("unsupported account resource version")
		}
		if r.Status == "pending" && r.PendingExpiresAt == nil {
			expires := time.Now().UTC().Add(contributionPendingTTL)
			r.PendingExpiresAt = &expires
			updates = append(updates, r)
		}
		var fields map[string]json.RawMessage
		if err := json.Unmarshal(v, &fields); err != nil {
			return err
		}
		if _, ok := fields["controls"]; !ok {
			updates = append(updates, r)
		}
		if err := r.Controls.validate(); err != nil {
			return err
		}
		if r.ID == "" || r.OwnerID == "" || r.Revision == 0 || resourceKey(r.Provider, r.ID) != string(k) || (r.Status != "pending" && r.Status != "active") {
			return errors.New("invalid account resource")
		}
		return nil
	}); err != nil {
		return err
	}
	for _, r := range updates {
		if err := putJSON(b, resourceKey(r.Provider, r.ID), r); err != nil {
			return err
		}
	}
	return nil
}

type accountResource struct {
	OperatorMayDelegate bool            `json:"operator_may_delegate"`
	Controls            accountControls `json:"controls"`
	Version             int             `json:"version"`
	ID                  string          `json:"id"`
	Provider            AccountType     `json:"provider"`
	OwnerID             string          `json:"owner_id"`
	AddedBy             string          `json:"added_by"`
	OperatorManaged     bool            `json:"operator_managed"`
	SecretRef           string          `json:"secret_ref"`
	Identity            string          `json:"identity,omitempty"`
	Revision            uint64          `json:"revision"`
	Status              string          `json:"status"`
	PendingExpiresAt    *time.Time      `json:"pending_expires_at,omitempty"`
	WithdrawnAt         *time.Time      `json:"withdrawn_at,omitempty"`
}

func resourceKey(provider AccountType, id string) string { return string(provider) + "|" + id }

func readAccountResource(b *bbolt.Bucket, provider AccountType, id string) (*accountResource, error) {
	if b == nil {
		return nil, errors.New("account authority unavailable")
	}
	raw := b.Get([]byte(resourceKey(provider, id)))
	if raw == nil {
		return nil, errors.New("account ownership missing")
	}
	var resource accountResource
	if err := json.Unmarshal(raw, &resource); err != nil {
		return nil, err
	}
	if resource.Version != 2 || resource.ID != id || resource.Provider != provider || resource.OwnerID == "" || resource.Revision == 0 {
		return nil, errors.New("invalid account ownership")
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	if _, ok := fields["controls"]; !ok {
		return nil, errors.New("account controls missing; migration required")
	}
	if err := resource.Controls.validate(); err != nil {
		return nil, err
	}
	return &resource, nil
}

func (p *PassportStore) initializeAccountAuthority(accounts []*Account) error {
	return p.db.Update(func(tx *bbolt.Tx) error {
		b, err := tx.CreateBucketIfNotExists([]byte(bucketAccountResources))
		if err != nil {
			return err
		}
		state := tx.Bucket([]byte(bucketAnalyticsState))
		if state.Get([]byte("account_ownership_migrated")) != nil {
			return nil
		}
		for _, a := range accounts {
			if b.Get([]byte(resourceKey(a.Type, a.ID))) != nil {
				continue
			}
			r := accountResource{Version: 2, ID: a.ID, Provider: a.Type, OwnerID: "operator-managed", AddedBy: "operator-managed", OperatorManaged: true, SecretRef: a.File, Revision: 1, Status: "active"}
			identity := a.AccessToken
			switch a.Type {
			case AccountTypeCodex:
				identity = a.AccountID
			case AccountTypeClaude:
				identity = a.AccountUUID
			case AccountTypeAntigravity:
				identity = a.Email
			case AccountTypeGrok:
				identity = a.RefreshToken
			}
			if identity != "" {
				hash := sha256.Sum256([]byte(string(a.Type) + "|" + identity))
				r.Identity = hex.EncodeToString(hash[:])
			}
			if err := putJSON(b, resourceKey(a.Type, a.ID), &r); err != nil {
				return err
			}
		}
		return state.Put([]byte("account_ownership_migrated"), []byte{1})
	})
}

func (p *PassportStore) accountActor(identity string) string {
	principalID, clientID := splitClientIdentity(identity)
	p.mu.RLock()
	defer p.mu.RUnlock()
	if clientID != "" {
		if client := p.clients[clientID]; client != nil && client.PrincipalID == principalID {
			return principalID
		}
		return ""
	}
	if client := p.clients[identity]; client != nil {
		return client.PrincipalID
	}
	return identity
}

func (h *proxyHandler) requestVisiblePool(r *http.Request) *poolState {
	if h.passport != nil {
		if principal, _ := h.passport.authenticate(r); principal != nil {
			return h.catalogPool(principal.ID)
		}
	}
	identity, _, _, _, allowed := h.authorizePoolCredentialRequest(r)
	if allowed {
		return h.catalogPool(identity)
	}
	return h.catalogPool("")
}

func (p *PassportStore) authorizeAccount(identity string, a *Account, action string) error {
	if p == nil || a == nil {
		return errors.New("account authority unavailable")
	}
	actor := p.accountActor(identity)
	principal := p.principal(actor)
	if identity != "" && identity != "break-glass" && (principal == nil || principal.Status != PrincipalActive || principal.ExpiresAt != nil && !principal.ExpiresAt.After(time.Now())) {
		return errors.New("account actor unavailable")
	}
	return p.db.View(func(tx *bbolt.Tx) error {
		r, err := readAccountResource(tx.Bucket([]byte(bucketAccountResources)), a.Type, a.ID)
		if err != nil {
			return err
		}
		if r.WithdrawnAt != nil {
			return errors.New("account withdrawn")
		}
		if r.Status != "active" {
			return errors.New("account is not active")
		}
		switch action {
		case "read", "use":
			if action == "use" && (r.Controls.State == accountDisabled || r.Controls.State == accountMaintenance) {
				return errors.New("account administratively unavailable")
			}
			if actor != "" && actor == r.OwnerID || r.OperatorManaged && (identity == "break-glass" || principal != nil && principal.Kind == PrincipalOperator) {
				return nil
			}
			grant, err := findAccountGrant(tx, r, actor, "")
			if err != nil {
				return err
			}
			if grant != nil {
				return nil
			}
		case "manage", "withdraw":
			if actor != "" && actor == r.OwnerID || r.OperatorManaged && (identity == "break-glass" || principal != nil && principal.Kind == PrincipalOperator) {
				return nil
			}
		default:
			return errors.New("unknown account permission")
		}
		return errors.New("account access denied")
	})
}

func (p *poolState) accountExclusions(identity string, excluded map[string]bool, models ...string) map[string]bool {
	result := make(map[string]bool, len(excluded))
	for id, value := range excluded {
		result[id] = value
	}
	if p.accountAuthority == nil {
		return result
	}
	for _, a := range p.allAccounts() {
		if p.accountAuthority.authorizeAccount(identity, a, "use") != nil {
			result[a.ID] = true
		}
		if len(models) > 0 && models[0] != "" {
			if _, err := p.accountAuthority.accountGrantForUse(identity, a, models[0]); err != nil {
				result[a.ID] = true
			}
		}
	}
	return result
}

func (p *poolState) visiblePool(identity string) *poolState {
	accounts := []*Account{}
	for _, a := range p.allAccounts() {
		if p.accountAuthority == nil || p.accountAuthority.authorizeAccount(identity, a, "read") == nil {
			accounts = append(accounts, a)
		}
	}
	visible := newPoolState(accounts, false)
	visible.catalogScoped = p.accountAuthority != nil
	if p.accountAuthority != nil {
		visible.catalogAccountAllows = func(a *Account, model string) bool {
			_, err := p.accountAuthority.accountGrantForUse(identity, a, model)
			return err == nil
		}
	}
	return visible
}

func (h *proxyHandler) catalogPool(identity string) *poolState {
	visible := h.pool.visiblePool(identity)
	authorize := visible.catalogAccountAllows
	visible.catalogAccountAllows = func(a *Account, model string) bool {
		return (authorize == nil || authorize(a, model)) && h.checkLivePolicy(identity, model, a.Type) == nil
	}
	return visible
}

func (h *proxyHandler) checkAccountUse(identity string, a *Account) error {
	if h.pool == nil || h.pool.accountAuthority == nil {
		return nil
	}
	if err := h.pool.accountAuthority.authorizeAccount(identity, a, "use"); err != nil {
		return &policyError{Status: http.StatusForbidden, Code: "account_access_denied", Message: fmt.Sprintf("account authorization failed: %v", err)}
	}
	return nil
}
