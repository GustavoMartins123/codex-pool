package main

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"go.etcd.io/bbolt"
)

// Operator access is personal, explicit and independent of delegation grants.
type operatorAccountAccess struct {
	Enabled   bool      `json:"enabled"`
	UpdatedAt time.Time `json:"updated_at"`
	Reason    string    `json:"reason"`
}

func (r *accountResource) validateOperatorAccess() error {
	for id, access := range r.OperatorAccess {
		if id == "" || id == r.OwnerID || r.OperatorManaged || access.UpdatedAt.IsZero() || strings.TrimSpace(access.Reason) == "" || len(access.Reason) > 240 {
			return errors.New("invalid operator account access")
		}
	}
	return nil
}

func accountPrincipal(tx *bbolt.Tx, actor string) (*Principal, error) {
	b := tx.Bucket([]byte(bucketPrincipals))
	if b == nil {
		return nil, errors.New("account principal authority unavailable")
	}
	var pr Principal
	raw := b.Get([]byte(actor))
	if raw == nil || json.Unmarshal(raw, &pr) != nil || pr.ID != actor || pr.Status != PrincipalActive || pr.ExpiresAt != nil && !pr.ExpiresAt.After(time.Now()) {
		return nil, accountControlError("account_access_denied", 403)
	}
	return &pr, nil
}

// All selectors and admissions share this decision, including model grants.
func accountReadPermission(tx *bbolt.Tx, r *accountResource, actor, identity, model string) (*accountGrant, error) {
	if r.WithdrawnAt != nil || r.Status != "active" {
		return nil, accountControlError("account_access_denied", 403)
	}
	var pr *Principal
	if identity != "break-glass" {
		var err error
		pr, err = accountPrincipal(tx, actor)
		if err != nil {
			return nil, err
		}
	}
	if actor != "" && actor == r.OwnerID || r.OperatorManaged && (identity == "break-glass" || principalIsUsableOperator(pr)) {
		return nil, nil
	}
	if principalIsUsableOperator(pr) && r.OperatorAccess[actor].Enabled {
		return nil, nil
	}
	grant, err := findAccountGrant(tx, r, actor, model)
	if err != nil {
		return nil, err
	}
	if grant == nil {
		return nil, accountControlError("account_grant_denied", 403)
	}
	return grant, nil
}

func (p *PassportStore) setOperatorAccountAccess(actor string, provider AccountType, id string, revision uint64, enabled bool, reason string) error {
	reason = strings.TrimSpace(reason)
	if reason == "" || len(reason) > 240 {
		return accountControlError("operator_access_reason_required", 400)
	}
	p.accountMu.Lock()
	defer p.accountMu.Unlock()
	return p.db.Update(func(tx *bbolt.Tx) error {
		pr, err := accountPrincipal(tx, actor)
		if err != nil || !principalIsUsableOperator(pr) {
			return accountControlError("operator_access_denied", 403)
		}
		b := tx.Bucket([]byte(bucketAccountResources))
		r, err := readAccountResource(b, provider, id)
		if err != nil {
			return err
		}
		if r.OperatorManaged || r.OwnerID == actor || r.Status != "active" || r.WithdrawnAt != nil {
			return accountControlError("operator_access_denied", 403)
		}
		if revision == 0 || revision != r.Revision {
			return accountControlError("account_revision_conflict", 409)
		}
		if r.OperatorAccess == nil {
			r.OperatorAccess = make(map[string]operatorAccountAccess)
		}
		r.OperatorAccess[actor] = operatorAccountAccess{Enabled: enabled, UpdatedAt: time.Now().UTC(), Reason: reason}
		r.Revision++
		if err := putJSON(b, resourceKey(provider, id), r); err != nil {
			return err
		}
		action := "account.operator_access_disabled"
		if enabled {
			action = "account.operator_access_enabled"
		}
		return p.audit(tx, actor, action, id, string(provider)+": "+reason)
	})
}

// Called in the same transaction as suspension/demotion. Restoring the role
// must never resurrect a previous activation.
func (p *PassportStore) revokeOperatorAccountAccess(tx *bbolt.Tx, actor, operator string) error {
	b := tx.Bucket([]byte(bucketAccountResources))
	if b == nil {
		return errors.New("account authority unavailable")
	}
	var updates []*accountResource
	if err := b.ForEach(func(k, v []byte) error {
		var key accountResource
		if err := json.Unmarshal(v, &key); err != nil {
			return err
		}
		r, err := readAccountResource(b, key.Provider, key.ID)
		if err != nil {
			return err
		}
		access, ok := r.OperatorAccess[operator]
		if !ok || !access.Enabled {
			return nil
		}
		access.Enabled = false
		access.UpdatedAt = time.Now().UTC()
		access.Reason = "Operator suspended or role removed"
		r.OperatorAccess[operator] = access
		r.Revision++
		updates = append(updates, r)
		return nil
	}); err != nil {
		return err
	}
	for _, r := range updates {
		if err := putJSON(b, resourceKey(r.Provider, r.ID), r); err != nil {
			return err
		}
		if err := p.audit(tx, actor, "account.operator_access_revoked", r.ID, string(r.Provider)+" "+operator); err != nil {
			return err
		}
	}
	return nil
}
