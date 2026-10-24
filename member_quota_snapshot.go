package main

import (
	"strings"
	"time"

	"go.etcd.io/bbolt"
)

type memberQuotaAccess struct {
	allModels bool
	usable    bool
	grants    []accountGrant
}

func (a memberQuotaAccess) allows(model string) bool {
	if a.allModels {
		return true
	}
	for _, grant := range a.grants {
		if policyAllows(PolicySelector{Allow: grant.Models}, model) {
			return true
		}
	}
	return false
}

// Authorization is read once per request, never retained across requests. All
// resources and model grants come from the same database read transaction.
func (h *proxyHandler) memberQuotaSnapshot(identity string, now time.Time) (*poolState, error) {
	principalID, clientID := splitClientIdentity(identity)
	principal, policy, _, err := h.passport.evaluateClientPolicy(principalID, clientID, h.cfg.hotClientPolicies())
	if err != nil {
		return nil, err
	}
	admission := &policyAdmission{policy: policy}
	accounts := h.pool.allAccounts()
	access := make(map[*Account]memberQuotaAccess, len(accounts))
	var verifications map[string]modelVerification
	err = h.passport.db.View(func(tx *bbolt.Tx) error {
		var err error
		verifications, err = readModelVerifications(tx, now)
		if err != nil {
			return err
		}
		for _, account := range accounts {
			resource, err := readAccountResource(tx.Bucket([]byte(bucketAccountResources)), account.Type, account.ID)
			if err != nil {
				return err
			}
			if resource.WithdrawnAt != nil || resource.Status != "active" || admission.CheckProvider(account.Type) != nil {
				continue
			}
			a := memberQuotaAccess{
				allModels: resource.OwnerID == principal.ID || resource.OperatorManaged && principal.Kind == PrincipalOperator,
				usable:    resource.Controls.State != accountDisabled && resource.Controls.State != accountMaintenance,
			}
			if !a.allModels {
				grants, err := accountGrantsByResource(tx, account.Type, account.ID, principal.ID)
				if err != nil {
					return err
				}
				for _, grant := range grants {
					if grantActive(grant, resource, principal.ID, now) {
						a.grants = append(a.grants, grant)
					}
				}
				if len(a.grants) == 0 {
					continue
				}
			}
			access[account] = a
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	// Antigravity requires an actual discovery for this account. The global
	// catalog's substitute entries must never establish a member entitlement.
	antigravityEntitlements := make(map[string]map[string]bool)
	antigravityModels.mu.RLock()
	for account := range access {
		if account.Type != AccountTypeAntigravity {
			continue
		}
		models := make(map[string]bool)
		for id := range antigravityModels.accounts[account.ID].Models {
			models[id] = true
		}
		antigravityEntitlements[account.ID] = models
	}
	antigravityModels.mu.RUnlock()

	// Copy only catalog and quota fields, without credentials or a copied mutex.
	// Each live account is locked once; later model scans use detached values.
	visible := &poolState{catalogScoped: true, catalogVerifications: verifications}
	detachedAccess := make(map[*Account]memberQuotaAccess, len(access))
	for _, account := range accounts {
		a, ok := access[account]
		if !ok {
			continue
		}
		account.mu.Lock()
		copy := &Account{
			ID: account.ID, Type: account.Type, Usage: account.Usage,
			Dead: account.Dead, Disabled: account.Disabled || !a.usable,
			NeedsVerification: account.NeedsVerification, VerificationURL: account.VerificationURL,
			HealthError: account.HealthError, RateLimitUntil: account.RateLimitUntil,
			Models:          make(map[string]DiscoveredModel, len(account.Models)),
			ModelRateLimits: make(map[string]time.Time, len(account.ModelRateLimits)),
		}
		for id, model := range account.Models {
			if admission.CheckModel(id) == nil && a.allows(id) {
				model.Modalities = append([]string(nil), model.Modalities...)
				copy.Models[id] = model
			}
		}
		for id, until := range account.ModelRateLimits {
			copy.ModelRateLimits[id] = until
		}
		account.mu.Unlock()
		visible.accounts = append(visible.accounts, copy)
		detachedAccess[copy] = a
	}
	visible.catalogAccountAllows = func(account *Account, model string) bool {
		a, ok := detachedAccess[account]
		if account.Type == AccountTypeAntigravity && !antigravityEntitlements[account.ID][strings.TrimPrefix(model, "antigravity/")] {
			return false
		}
		return ok && admission.CheckModel(model) == nil && a.allows(model)
	}
	return visible, nil
}
