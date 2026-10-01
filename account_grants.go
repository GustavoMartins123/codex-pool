package main

import (
	"encoding/json"
	"errors"
	"strings"
	"time"

	"go.etcd.io/bbolt"
)

const bucketAccountGrants = "account_grants"

type accountGrant struct {
	ID          string       `json:"id"`
	Provider    AccountType  `json:"provider"`
	AccountID   string       `json:"account_id"`
	RecipientID string       `json:"recipient_id"`
	Models      []string     `json:"models"`
	Budget      PolicyLimits `json:"budget"`
	ExpiresAt   time.Time    `json:"expires_at"`
	CreatedAt   time.Time    `json:"created_at"`
	CreatedBy   string       `json:"created_by"`
	Reason      string       `json:"reason"`
	Revision    uint64       `json:"revision"`
	RevokedAt   *time.Time   `json:"revoked_at,omitempty"`
}

func (g accountGrant) validate() error {
	if g.ID == "" || len(g.ID) > 80 || g.AccountID == "" || g.Provider == "" || g.RecipientID == "" || g.CreatedBy == "" || g.Revision == 0 || g.ExpiresAt.IsZero() || g.CreatedAt.IsZero() || !g.ExpiresAt.After(g.CreatedAt) || g.RevokedAt != nil && g.RevokedAt.Before(g.CreatedAt) {
		return errors.New("invalid account grant")
	}
	for _, c := range g.ID {
		if c != '-' && c != '_' && !(c >= '0' && c <= '9') && !(c >= 'a' && c <= 'z') && !(c >= 'A' && c <= 'Z') {
			return errors.New("invalid grant ID")
		}
	}
	if len(g.Models) == 0 || strings.TrimSpace(g.Reason) == "" || len(g.Reason) > 240 {
		return errors.New("models and reason are required")
	}
	if err := validateClientPolicy(ClientPolicy{Models: PolicySelector{Allow: g.Models}, Limits: g.Budget}); err != nil {
		return err
	}
	if g.Budget.DailyRequests == 0 && g.Budget.MonthlyRequests == 0 && g.Budget.DailyTokens == 0 && g.Budget.MonthlyTokens == 0 {
		return errors.New("an explicit daily or monthly budget is required")
	}
	return nil
}

func readAccountGrants(b *bbolt.Bucket) ([]accountGrant, error) {
	if b == nil {
		return nil, errors.New("account grants unavailable")
	}
	out := []accountGrant{}
	err := b.ForEach(func(k, v []byte) error {
		var g accountGrant
		if err := json.Unmarshal(v, &g); err != nil {
			return err
		}
		if err := g.validate(); err != nil {
			return err
		}
		if g.ID != string(k) {
			return errors.New("invalid grant key")
		}
		out = append(out, g)
		return nil
	})
	return out, err
}

func grantActive(g accountGrant, resource *accountResource, actor string, now time.Time) bool {
	return g.RecipientID == actor && g.Provider == resource.Provider && g.AccountID == resource.ID && g.RevokedAt == nil && g.ExpiresAt.After(now) && resource.WithdrawnAt == nil && resource.Status == "active" && (resource.OperatorManaged || g.CreatedBy == resource.OwnerID || resource.OperatorMayDelegate)
}

func findAccountGrant(tx *bbolt.Tx, resource *accountResource, actor, model string) (*accountGrant, error) {
	grants, err := accountGrantsByResource(tx, resource.Provider, resource.ID, actor)
	if err != nil {
		return nil, err
	}
	for _, g := range grants {
		if grantActive(g, resource, actor, time.Now()) && (model == "" || policyAllows(PolicySelector{Allow: g.Models}, model)) {
			return &g, nil
		}
	}
	return nil, nil
}

func (p *PassportStore) canDelegate(actor string, r *accountResource) bool {
	principal := p.principal(actor)
	return principal != nil && principal.Status == PrincipalActive && (principal.ExpiresAt == nil || principal.ExpiresAt.After(time.Now())) && (actor == r.OwnerID || principal.Kind == PrincipalOperator && (r.OperatorManaged || r.OperatorMayDelegate))
}

func (p *PassportStore) setAccountDelegation(actor string, provider AccountType, id string, revision uint64, allowed bool) error {
	p.accountMu.Lock()
	defer p.accountMu.Unlock()
	return p.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(bucketAccountResources))
		resource, err := readAccountResource(b, provider, id)
		if err != nil {
			return err
		}
		if resource.OwnerID != actor || resource.OperatorManaged || resource.WithdrawnAt != nil {
			return accountControlError("account_access_denied", 403)
		}
		if revision == 0 || resource.Revision != revision {
			return accountControlError("account_revision_conflict", 409)
		}
		resource.OperatorMayDelegate = allowed
		if !allowed {
			grants, err := accountGrantsByResource(tx, provider, id, "")
			if err != nil {
				return err
			}
			now := time.Now().UTC()
			for _, grant := range grants {
				if grant.Provider == provider && grant.AccountID == id && grant.CreatedBy != actor && grant.RevokedAt == nil {
					grant.RevokedAt = &now
					grant.Revision++
					if err := putAccountGrant(tx, grant); err != nil {
						return err
					}
				}
			}
		}
		resource.Revision++
		if err := putJSON(b, resourceKey(provider, id), resource); err != nil {
			return err
		}
		return p.audit(tx, actor, "account.delegation_updated", id, string(provider))
	})
}

func (p *PassportStore) createAccountGrant(actor string, revision uint64, grant accountGrant) (*accountGrant, error) {
	grant.CreatedBy = actor
	grant.CreatedAt = time.Now().UTC()
	grant.Revision = 1
	if err := grant.validate(); err != nil {
		return nil, &policyError{Status: 400, Code: "grant_invalid", Message: err.Error()}
	}
	if !grant.ExpiresAt.After(grant.CreatedAt) || grant.ExpiresAt.After(grant.CreatedAt.Add(365*24*time.Hour)) {
		return nil, accountControlError("grant_expiry_invalid", 400)
	}
	if (grant.Budget.DailyTokens > 0 || grant.Budget.MonthlyTokens > 0) && grant.Provider != AccountTypeCodex && grant.Provider != AccountTypeClaude {
		return nil, unboundedPolicyRequest()
	}
	recipient := p.principal(grant.RecipientID)
	if recipient == nil || recipient.Status != PrincipalActive || recipient.ExpiresAt != nil && !recipient.ExpiresAt.After(time.Now()) {
		return nil, accountControlError("grant_recipient_unavailable", 400)
	}
	p.accountMu.Lock()
	defer p.accountMu.Unlock()
	var resource *accountResource
	if err := p.db.View(func(tx *bbolt.Tx) error {
		var err error
		resource, err = readAccountResource(tx.Bucket([]byte(bucketAccountResources)), grant.Provider, grant.AccountID)
		return err
	}); err != nil {
		return nil, err
	}
	if !p.canDelegate(actor, resource) || resource.WithdrawnAt != nil || resource.Status != "active" || recipient.ID == resource.OwnerID {
		return nil, accountControlError("account_delegation_denied", 403)
	}
	err := p.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(bucketAccountGrants))
		existing, err := readAccountGrant(b, grant.ID)
		if err != nil {
			return err
		}
		if existing != nil && (existing.Provider != grant.Provider || existing.AccountID != grant.AccountID || existing.RecipientID != grant.RecipientID) {
			return accountControlError("grant_id_conflict", 409)
		}
		grants, err := accountGrantsByResource(tx, grant.Provider, grant.AccountID, grant.RecipientID)
		if err != nil {
			return err
		}
		for _, existing := range grants {
			if existing.ID == grant.ID {
				left, right := existing, grant
				left.CreatedAt = right.CreatedAt
				left.Revision = right.Revision
				left.RevokedAt = nil
				l, _ := json.Marshal(left)
				r, _ := json.Marshal(right)
				if existing.RevokedAt != nil || string(l) != string(r) {
					return accountControlError("grant_id_conflict", 409)
				}
				grant = existing
				return nil
			}
			if existing.RecipientID == grant.RecipientID && existing.Provider == grant.Provider && existing.AccountID == grant.AccountID && existing.RevokedAt == nil && existing.ExpiresAt.After(time.Now()) {
				for _, model := range grant.Models {
					if policyMatches(existing.Models, model) || model == "*" {
						return accountControlError("grant_models_overlap", 409)
					}
				}
				if policyMatches(existing.Models, "*") {
					return accountControlError("grant_models_overlap", 409)
				}
			}
		}
		if resource.Revision != revision || revision == 0 {
			return accountControlError("account_revision_conflict", 409)
		}
		resource.Revision++
		if err := putJSON(tx.Bucket([]byte(bucketAccountResources)), resourceKey(resource.Provider, resource.ID), resource); err != nil {
			return err
		}
		if err := putAccountGrant(tx, grant); err != nil {
			return err
		}
		return p.audit(tx, actor, "account.grant_created", grant.ID, string(grant.Provider)+" "+grant.AccountID+" "+grant.RecipientID)
	})
	return &grant, err
}

func (p *PassportStore) revokeAccountGrant(actor, id string, revision uint64) error {
	p.accountMu.Lock()
	defer p.accountMu.Unlock()
	var grant accountGrant
	var resource *accountResource
	err := p.db.View(func(tx *bbolt.Tx) error {
		stored, err := readAccountGrant(tx.Bucket([]byte(bucketAccountGrants)), id)
		if err != nil {
			return err
		}
		if stored == nil {
			return accountControlError("grant_not_found", 404)
		}
		grant = *stored
		resource, err = readAccountResource(tx.Bucket([]byte(bucketAccountResources)), grant.Provider, grant.AccountID)
		return err
	})
	if err != nil {
		return err
	}
	if actor != resource.OwnerID && actor != grant.CreatedBy && !p.canDelegate(actor, resource) {
		return accountControlError("account_delegation_denied", 403)
	}
	if grant.RevokedAt != nil {
		return nil
	}
	if revision == 0 || grant.Revision != revision {
		return accountControlError("grant_revision_conflict", 409)
	}
	now := time.Now().UTC()
	grant.RevokedAt = &now
	grant.Revision++
	return p.db.Update(func(tx *bbolt.Tx) error {
		if err := putAccountGrant(tx, grant); err != nil {
			return err
		}
		return p.audit(tx, actor, "account.grant_revoked", id, string(grant.Provider)+" "+grant.AccountID)
	})
}

func (p *PassportStore) accountGrantForUse(identity string, a *Account, model string) (*accountGrant, error) {
	actor := p.accountActor(identity)
	principal := p.principal(actor)
	if identity != "break-glass" && (principal == nil || principal.Status != PrincipalActive || principal.ExpiresAt != nil && !principal.ExpiresAt.After(time.Now())) {
		return nil, accountControlError("account_access_denied", 403)
	}
	var grant *accountGrant
	err := p.db.View(func(tx *bbolt.Tx) error {
		r, err := readAccountResource(tx.Bucket([]byte(bucketAccountResources)), a.Type, a.ID)
		if err != nil {
			return err
		}
		if r.WithdrawnAt != nil || r.Status != "active" {
			return accountControlError("account_access_denied", 403)
		}
		if actor == r.OwnerID || r.OperatorManaged && (identity == "break-glass" || principal != nil && principal.Kind == PrincipalOperator) {
			return nil
		}
		grant, err = findAccountGrant(tx, r, actor, model)
		if err != nil {
			return err
		}
		if grant == nil {
			return accountControlError("account_grant_denied", 403)
		}
		return nil
	})
	return grant, err
}

func (p *PassportStore) reserveGrantRequest(grant *accountGrant, now time.Time) (*policyAdmission, error) {
	p.policyMu.Lock()
	defer p.policyMu.Unlock()
	scope := "grant:" + grant.ID
	if grant.Budget.ConcurrentRequests > 0 && p.policyInflight[scope] >= grant.Budget.ConcurrentRequests {
		return nil, accountControlError("grant_concurrency_exceeded", 429)
	}
	minuteKey, dayKey, monthKey := policyUsageKeys(scope, now)
	a := &policyAdmission{store: p, principalID: grant.RecipientID, policy: ClientPolicy{Models: PolicySelector{Allow: grant.Models}, Limits: grant.Budget}}
	err := p.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(bucketPassportPolicyUsage))
		minute, err := readPolicyCounter(b, minuteKey)
		if err != nil {
			return err
		}
		day, err := readPolicyCounter(b, dayKey)
		if err != nil {
			return err
		}
		month, err := readPolicyCounter(b, monthKey)
		if err != nil {
			return err
		}
		if err := checkBudget(grant.Budget, minute, day, month); err != nil {
			return err
		}
		reserved := policyTokenReservation(grant.Budget, day, month, 0)
		minute.Requests++
		day.Requests++
		month.Requests++
		day.ReservedTokens += reserved
		month.ReservedTokens += reserved
		for k, v := range map[string]policyUsageCounter{minuteKey: minute, dayKey: day, monthKey: month} {
			if err := writePolicyCounter(b, k, v); err != nil {
				return err
			}
		}
		a.holds = []policyBudgetHold{{Scope: scope, Day: dayKey, Month: monthKey, Tokens: reserved, Limits: grant.Budget}}
		return nil
	})
	if err != nil {
		return nil, err
	}
	p.policyInflight[scope]++
	p.policyReserved[scope] += a.holds[0].Tokens
	return a, nil
}
