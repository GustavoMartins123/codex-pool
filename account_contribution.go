package main

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"codex-pool-proxy/internal/credstore"
	"go.etcd.io/bbolt"
)

func contributionDirectory(provider AccountType) (string, error) {
	switch provider {
	case AccountTypeCodex, AccountTypeClaude, AccountTypeAntigravity, AccountTypeKimi, AccountTypeMinimax, AccountTypeZAI, AccountTypeXiaomi, AccountTypeGrok, AccountTypeAdverserial, AccountTypeQwen, AccountTypeAIStudio:
		return string(provider), nil
	case AccountTypeOpencodeGo:
		return "opencode_go", nil
	default:
		return "", errors.New("provider does not accept contributions")
	}
}

func (p *PassportStore) contributionActorAllowed(actor string) bool {
	if actor == "break-glass" {
		return true
	}
	pr := p.principal(actor)
	return pr != nil && pr.Status == PrincipalActive && (pr.ExpiresAt == nil || pr.ExpiresAt.After(time.Now())) && (pr.Kind == PrincipalOperator || pr.Kind == PrincipalMember && pr.CanContribute)
}

// contributionIdentity is the provider account a contribution registers.
// Legacy is the identity an earlier scheme recorded for the same account; a
// resource registered under it still counts as this account unless its ID is
// in Distinct, which callers fill with resources proven to be someone else.
type contributionIdentity struct {
	Value    string
	Legacy   string
	Distinct map[string]bool
}

func contributionFingerprint(provider AccountType, identity string) string {
	hash := sha256.Sum256([]byte(string(provider) + "|" + identity))
	return hex.EncodeToString(hash[:])
}

func (h *proxyHandler) persistContribution(actor string, provider AccountType, ident contributionIdentity, write func(string) error) (string, error) {
	if h.passport == nil || h.cfg == nil || !h.passport.contributionActorAllowed(actor) {
		return "", errors.New("account contribution is not authorized")
	}
	if keyed, ok := h.cfg.vault.store.(*credstore.KeyedStore); !ok || keyed == nil {
		return "", errors.New("encrypted credential vault is required")
	}
	dir, err := contributionDirectory(provider)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(ident.Value) == "" {
		return "", errors.New("provider account identity is required")
	}
	fingerprint := contributionFingerprint(provider, ident.Value)
	legacyFingerprint := ""
	if strings.TrimSpace(ident.Legacy) != "" && ident.Legacy != ident.Value {
		legacyFingerprint = contributionFingerprint(provider, ident.Legacy)
	}
	id := dir + "_" + fingerprint[:24]
	ref := filepath.Join(dir, id+".json")
	path := filepath.Join(h.cfg.poolDir, ref)
	p := h.passport
	p.contributionMu.Lock()
	defer p.contributionMu.Unlock()
	if !p.contributionActorAllowed(actor) {
		return "", errors.New("account contribution is not authorized")
	}
	err = p.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(bucketAccountResources))
		if b == nil {
			return errors.New("account authority unavailable")
		}
		key := resourceKey(provider, id)
		count := 0
		if err := b.ForEach(func(k, v []byte) error {
			var existing accountResource
			if err := json.Unmarshal(v, &existing); err != nil {
				return err
			}
			if existing.OwnerID == actor && existing.WithdrawnAt == nil && (existing.Status == "active" || existing.Status == "pending" && existing.PendingExpiresAt != nil && existing.PendingExpiresAt.After(time.Now())) {
				count++
			}
			if existing.Provider == provider && string(k) != key && (existing.Identity == fingerprint || legacyFingerprint != "" && existing.Identity == legacyFingerprint && !ident.Distinct[existing.ID]) {
				return &policyError{Status: 409, Code: "account_duplicate", Message: "provider account already registered"}
			}
			return nil
		}); err != nil {
			return err
		}
		if b.Get([]byte(key)) != nil {
			r, err := readAccountResource(b, provider, id)
			if err != nil {
				return err
			}
			if r.OwnerID != actor || r.Identity != fingerprint || r.WithdrawnAt != nil || r.Status != "pending" {
				return &policyError{Status: http.StatusConflict, Code: "account_duplicate", Message: "provider account already registered"}
			}
			if r.PendingExpiresAt == nil || !r.PendingExpiresAt.After(time.Now()) {
				return &policyError{Status: 409, Code: "contribution_expired", Message: "pending account registration expired"}
			}
			return nil
		}
		if count >= contributionAccountsPerPrincipal {
			return &policyError{Status: 429, Code: "contribution_account_limit", Message: "private account limit reached"}
		}
		expires := time.Now().UTC().Add(contributionPendingTTL)
		if err := putJSON(b, key, &accountResource{Version: 2, ID: id, Provider: provider, OwnerID: actor, AddedBy: actor, OperatorManaged: actor == "break-glass", SecretRef: ref, Identity: fingerprint, Revision: 1, Status: "pending", PendingExpiresAt: &expires}); err != nil {
			return err
		}
		// Re-adding a deleted account reuses the deterministic key; clear its
		// tombstone in the same tx so restore cannot drop the new resource.
		if deletions := tx.Bucket([]byte(bucketAccountDeletions)); deletions != nil {
			if err := deletions.Delete([]byte(key)); err != nil {
				return err
			}
		}
		return p.audit(tx, actor, "account.contribution_started", id, string(provider))
	})
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return "", err
	}
	if err := write(path); err != nil {
		return "", err
	}
	data, err := h.cfg.vault.readAccountFile(path)
	if err != nil {
		return "", err
	}
	if h.registry == nil || h.registry.ForType(provider) == nil {
		return "", errors.New("contribution provider is unavailable")
	}
	loaded, err := h.registry.ForType(provider).LoadAccount(filepath.Base(path), path, data)
	if err != nil {
		return "", err
	}
	if loaded == nil || loaded.AccessToken == "" {
		return "", errors.New("provider credentials are missing")
	}
	err = p.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(bucketAccountResources))
		r, err := readAccountResource(b, provider, id)
		if err != nil {
			return err
		}
		if r.Status != "pending" || r.WithdrawnAt != nil || r.PendingExpiresAt == nil || !r.PendingExpiresAt.After(time.Now()) {
			return errors.New("account contribution no longer authorized")
		}
		r.Status = "active"
		r.PendingExpiresAt = nil
		r.Revision++
		if err := putJSON(b, resourceKey(provider, id), r); err != nil {
			return err
		}
		return p.audit(tx, actor, "account.contributed", id, string(provider))
	})
	if err != nil {
		return "", err
	}
	if h.pool != nil {
		if err := h.reloadAccounts(); err != nil {
			return "", err
		}
	}
	return id, nil
}

func (h *proxyHandler) saveContribution(r *http.Request, provider AccountType, identity string, payload any) (string, error) {
	return h.saveContributionIdentity(r, provider, contributionIdentity{Value: identity}, payload)
}

func (h *proxyHandler) saveContributionIdentity(r *http.Request, provider AccountType, ident contributionIdentity, payload any) (string, error) {
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return h.persistContribution(providerContributionActor(r), provider, ident, func(path string) error { return h.cfg.vault.writeAccountFile(path, data) })
}

func (h *proxyHandler) handleContributionPolicy(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if h.passport == nil {
		respondJSONError(w, 503, "account authority unavailable")
		return
	}
	operator, session, ok := h.requireOperator(w, r)
	if !ok {
		return
	}
	if !h.passportCSRF(r, session) {
		respondJSONError(w, 403, "invalid CSRF token")
		return
	}
	if r.Method != http.MethodPut {
		http.Error(w, "method not allowed", 405)
		return
	}
	var q struct {
		PrincipalID   string `json:"principal_id"`
		CanContribute *bool  `json:"can_contribute"`
	}
	if decodeGovernanceJSON(w, r, &q) != nil || q.CanContribute == nil {
		respondJSONError(w, 400, "invalid json")
		return
	}
	p := h.passport
	p.contributionMu.Lock()
	defer p.contributionMu.Unlock()
	p.mu.Lock()
	defer p.mu.Unlock()
	pr := p.principals[q.PrincipalID]
	if pr == nil || pr.Kind != PrincipalMember {
		respondJSONError(w, 400, "member principal required")
		return
	}
	updated := *pr
	updated.CanContribute = *q.CanContribute
	if err := p.db.Update(func(tx *bbolt.Tx) error {
		pr, err := accountPrincipal(tx, operator.ID)
		if err != nil || !principalIsUsableOperator(pr) {
			return accountControlError("operator_access_denied", 403)
		}
		if err := putJSON(tx.Bucket([]byte(bucketPrincipals)), updated.ID, &updated); err != nil {
			return err
		}
		return p.audit(tx, operator.ID, "account.contribution_permission_changed", updated.ID, "")
	}); err != nil {
		respondJSONError(w, 500, "could not update account contribution permission")
		return
	}
	p.principals[updated.ID] = &updated
	respondJSON(w, map[string]any{"principal_id": updated.ID, "can_contribute": updated.CanContribute})
}
