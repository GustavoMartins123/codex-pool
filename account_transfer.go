package main

import (
	"crypto/hmac"
	"crypto/sha256"
	"crypto/subtle"
	"encoding/base64"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"regexp"
	"sort"
	"strings"
	"time"

	"go.etcd.io/bbolt"
)

// accountsExportFormat identifies the account list an export produces and an
// import accepts. The list carries no credential: tokens, cookies and keys
// stay on the pool that exported it.
const (
	accountsExportFormat  = "codex-pool-accounts"
	accountsExportVersion = 1
)

// accountExportProviders maps the providers whose accounts sign in through
// the pool's own login flows to their pool subdirectory. API-key providers are
// left out: they have no sign-in to wait for, and adding a key again is a
// single paste.
var accountExportProviders = map[AccountType]string{
	AccountTypeCodex:       "codex",
	AccountTypeClaude:      "claude",
	AccountTypeAntigravity: "antigravity",
}

type exportedAccount struct {
	Provider        string `json:"provider"`
	ID              string `json:"id"`
	Email           string `json:"email,omitempty"`
	PlanType        string `json:"plan_type,omitempty"`
	AddedAt         string `json:"added_at,omitempty"`
	OperatorManaged bool   `json:"operator_managed,omitempty"`
}

// accountsExport is the file an export writes and an import reads. Signature
// authenticates every other field: only a pool with the same signing secret
// produces a signature an import accepts.
type accountsExport struct {
	Format     string            `json:"format"`
	Version    int               `json:"version"`
	ExportedAt string            `json:"exported_at"`
	Accounts   []exportedAccount `json:"accounts"`
	Signature  string            `json:"signature,omitempty"`
}

// placeholderAccountFile is the credential-less account file an import writes.
func placeholderAccountFile(entry exportedAccount) map[string]any {
	root := map[string]any{
		placeholderAccountMarker: true,
		"needs_login":            true,
		"auth_error":             placeholderAccountAuthError,
	}
	if email := strings.TrimSpace(entry.Email); email != "" {
		root["email"] = email
	}
	if plan := strings.TrimSpace(entry.PlanType); plan != "" {
		root["plan_type"] = plan
	}
	if added := strings.TrimSpace(entry.AddedAt); added != "" {
		root["added_at"] = added
	}
	return root
}

// accountsExportSigningLabel separates the export signature from the tokens
// the same pool secret signs.
const accountsExportSigningLabel = "codex-pool-accounts-export/v1"

var (
	errExportSigningUnavailable = errors.New("account export signing is not configured on this pool")
	errExportSignature          = errors.New("account export signature is invalid: the file was not exported by a pool with this pool's signing secret, or it changed afterwards")
)

// signAccountsExport signs every field of the export with a key derived from
// the pool's JWT secret, so only a pool with the same secret can import it.
func signAccountsExport(file *accountsExport, secret string) error {
	if strings.TrimSpace(secret) == "" {
		return errExportSigningUnavailable
	}
	file.Signature = ""
	payload, err := json.Marshal(file)
	if err != nil {
		return err
	}
	file.Signature = base64.RawURLEncoding.EncodeToString(accountsExportMAC(payload, secret))
	return nil
}

// verifyAccountsExport accepts only a list that a pool with the same secret
// signed and that did not change afterwards. The list is decoded and encoded
// again before the check, so formatting differences in the file do not
// matter while any change to an account does.
func verifyAccountsExport(file accountsExport, secret string) error {
	if strings.TrimSpace(secret) == "" {
		return errExportSigningUnavailable
	}
	got, err := base64.RawURLEncoding.DecodeString(file.Signature)
	if err != nil {
		return errExportSignature
	}
	file.Signature = ""
	payload, err := json.Marshal(file)
	if err != nil {
		return err
	}
	if subtle.ConstantTimeCompare(got, accountsExportMAC(payload, secret)) != 1 {
		return errExportSignature
	}
	return nil
}

func accountsExportMAC(payload []byte, secret string) []byte {
	mac := hmac.New(sha256.New, []byte(secret))
	mac.Write([]byte(accountsExportSigningLabel))
	signingKey := mac.Sum(nil)
	mac = hmac.New(sha256.New, signingKey)
	mac.Write(payload)
	return mac.Sum(nil)
}

// maxImportedAccounts bounds one import so a crafted file cannot fill the pool
// directory.
const maxImportedAccounts = 500

// importedAccountIDPattern keeps an imported ID inside its own file name: no
// separator, no dot-only name, the length the pool already uses.
var importedAccountIDPattern = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._-]{0,119}$`)

var errAccountExists = errors.New("account already exists")

type accountsImportResult struct {
	Imported []string `json:"imported"`
	Skipped  []string `json:"skipped"`
	Invalid  []string `json:"invalid"`
}

// exportAccounts lists the sign-in accounts this actor manages. It reads no
// credential: each entry carries only what identifies the account on another
// pool. Accounts the provider cannot sign in again, dead accounts and
// accounts still waiting for a sign-in are left out.
func (h *proxyHandler) exportAccounts(actor string) accountsExport {
	out := accountsExport{
		Format:     accountsExportFormat,
		Version:    accountsExportVersion,
		ExportedAt: time.Now().UTC().Format(time.RFC3339),
		Accounts:   []exportedAccount{},
	}
	for _, acc := range h.pool.allAccounts() {
		dir, ok := accountExportProviders[acc.Type]
		if !ok || h.passport.authorizeAccount(actor, acc, "manage") != nil {
			continue
		}
		acc.mu.Lock()
		dead := acc.Dead || isPlaceholderAccountLocked(acc)
		entry := exportedAccount{
			Provider: dir,
			ID:       acc.ID,
			Email:    strings.TrimSpace(acc.Email),
			PlanType: strings.TrimSpace(acc.PlanType),
		}
		if !acc.AddedAt.IsZero() {
			entry.AddedAt = acc.AddedAt.UTC().Format(time.RFC3339)
		}
		acc.mu.Unlock()
		if dead {
			continue
		}
		if resource := h.passport.accountResourceOf(acc); resource != nil {
			entry.OperatorManaged = resource.OperatorManaged
		}
		out.Accounts = append(out.Accounts, entry)
	}
	sort.Slice(out.Accounts, func(i, j int) bool {
		if out.Accounts[i].Provider != out.Accounts[j].Provider {
			return out.Accounts[i].Provider < out.Accounts[j].Provider
		}
		return out.Accounts[i].ID < out.Accounts[j].ID
	})
	if err := signAccountsExport(&out, h.passportConfig.jwtSecret); err != nil {
		return accountsExport{}
	}
	return out
}

// importAccounts creates a waiting account for each entry of an export. An
// account the pool already has is skipped, never overwritten.
func (h *proxyHandler) importAccounts(actor string, entries []exportedAccount) (accountsImportResult, error) {
	result := accountsImportResult{Imported: []string{}, Skipped: []string{}, Invalid: []string{}}
	seen := make(map[string]bool, len(entries))
	for _, entry := range entries {
		accountType, ok := importableAccountType(entry.Provider)
		id := strings.TrimSpace(entry.ID)
		if !ok || !importedAccountIDPattern.MatchString(id) || len(entry.Email) > 254 || len(entry.PlanType) > 64 {
			result.Invalid = append(result.Invalid, strings.TrimSpace(entry.Provider)+"/"+id)
			continue
		}
		name := entry.Provider + "/" + id
		if seen[name] {
			result.Skipped = append(result.Skipped, name)
			continue
		}
		seen[name] = true
		switch err := h.importPlaceholderAccount(actor, accountType, entry.Provider, id, sanitizeImportedAccount(entry)); {
		case errors.Is(err, errAccountExists):
			result.Skipped = append(result.Skipped, name)
		case err != nil:
			return result, err
		default:
			result.Imported = append(result.Imported, name)
		}
	}
	return result, nil
}

func importableAccountType(provider string) (AccountType, bool) {
	for accountType, dir := range accountExportProviders {
		if dir == provider {
			return accountType, true
		}
	}
	return "", false
}

// sanitizeImportedAccount bounds the free-text fields an import stores.
func sanitizeImportedAccount(entry exportedAccount) exportedAccount {
	entry.Email = strings.TrimSpace(entry.Email)
	entry.PlanType = strings.TrimSpace(entry.PlanType)
	if added, err := time.Parse(time.RFC3339, strings.TrimSpace(entry.AddedAt)); err != nil || len(entry.AddedAt) > 64 {
		entry.AddedAt = ""
	} else {
		entry.AddedAt = added.UTC().Format(time.RFC3339)
	}
	return entry
}

// importPlaceholderAccount writes the credential-less account file and
// registers the account under the importing actor. The file comes first and is
// removed again when the registration fails, so the directory and the account
// authority never disagree.
func (h *proxyHandler) importPlaceholderAccount(actor string, accountType AccountType, dir, id string, entry exportedAccount) error {
	for _, acc := range h.pool.allAccounts() {
		if acc.Type == accountType && acc.ID == id {
			return errAccountExists
		}
	}
	ref := filepath.Join(dir, id+".json")
	path := filepath.Join(h.cfg.poolDir, ref)
	if _, err := os.Lstat(path); !errors.Is(err, os.ErrNotExist) {
		return errAccountExists
	}
	data, err := json.MarshalIndent(placeholderAccountFile(entry), "", "  ")
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	if err := h.cfg.vault.writeAccountFile(path, data); err != nil {
		return err
	}
	owner, operatorManaged := importedAccountOwner(actor, entry.OperatorManaged)
	key := resourceKey(accountType, id)
	err = h.passport.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(bucketAccountResources))
		if b == nil {
			return errors.New("account authority unavailable")
		}
		if b.Get([]byte(key)) != nil {
			return errAccountExists
		}
		resource := &accountResource{Version: 2, ID: id, Provider: accountType, OwnerID: owner, AddedBy: actor, OperatorManaged: operatorManaged, SecretRef: ref, Revision: 1, Status: "active"}
		if err := putJSON(b, key, resource); err != nil {
			return err
		}
		// A deleted account may have left a tombstone under this key; without
		// clearing it a later restore would drop the imported account.
		if deletions := tx.Bucket([]byte(bucketAccountDeletions)); deletions != nil {
			if err := deletions.Delete([]byte(key)); err != nil {
				return err
			}
		}
		return h.passport.audit(tx, actor, "account.imported", id, string(accountType))
	})
	if err != nil {
		os.Remove(path)
		return err
	}
	return nil
}

// importedAccountOwner decides who owns and may manage an imported account.
// The break-glass credential has no principal, so its accounts stay open to
// every operator.
func importedAccountOwner(actor string, operatorManaged bool) (owner string, managed bool) {
	if actor == "break-glass" {
		return actor, true
	}
	return actor, operatorManaged
}

func (p *PassportStore) accountResourceOf(a *Account) *accountResource {
	if p == nil || a == nil {
		return nil
	}
	var resource *accountResource
	p.db.View(func(tx *bbolt.Tx) error {
		resource, _ = readAccountResource(tx.Bucket([]byte(bucketAccountResources)), a.Type, a.ID)
		return nil
	})
	return resource
}
