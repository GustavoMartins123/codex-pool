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
	case AccountTypeCodex, AccountTypeClaude, AccountTypeAntigravity, AccountTypeKimi, AccountTypeMinimax, AccountTypeZAI, AccountTypeXiaomi, AccountTypeGrok, AccountTypeAdverserial:
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

func (h *proxyHandler) persistContribution(actor string, provider AccountType, identity string, write func(string) error) (string, error) {
	if h.passport == nil || h.cfg == nil || !h.passport.contributionActorAllowed(actor) {
		return "", errors.New("account contribution is not authorized")
	}
	if _, ok := accountCredentialStore.(*credstore.KeyedStore); !ok {
		return "", errors.New("encrypted credential vault is required")
	}
	dir, err := contributionDirectory(provider)
	if err != nil {
		return "", err
	}
	if strings.TrimSpace(identity) == "" {
		return "", errors.New("provider account identity is required")
	}
	hash := sha256.Sum256([]byte(string(provider) + "|" + identity))
	fingerprint := hex.EncodeToString(hash[:])
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
			if existing.Provider == provider && existing.Identity == fingerprint && string(k) != key {
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
	data, err := readAccountFile(path)
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
	data, err := json.Marshal(payload)
	if err != nil {
		return "", err
	}
	return h.persistContribution(providerContributionActor(r), provider, identity, func(path string) error { return writeAccountFile(path, data) })
}

func (h *proxyHandler) handleContributionPolicy(w http.ResponseWriter, r *http.Request) {
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
		CanContribute bool   `json:"can_contribute"`
	}
	if json.NewDecoder(http.MaxBytesReader(w, r.Body, 4096)).Decode(&q) != nil {
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
	updated.CanContribute = q.CanContribute
	if err := p.db.Update(func(tx *bbolt.Tx) error {
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
