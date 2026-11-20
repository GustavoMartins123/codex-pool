package main

import (
	"bytes"
	"encoding/json"
	"log"
	"net/http"
	"os"
	"strings"
	"sync/atomic"
	"time"

	"go.etcd.io/bbolt"
)

const bucketAccountDeletions = "account_deletions"

// accountDeletion is a tombstone keyed by resourceKey(provider, id). It records
// that an operator hard-deleted an account so a later restore cannot resurrect
// the ownership record from a backup taken before the delete. Re-contributing
// the same identity clears the tombstone in the creation transaction.
type accountDeletion struct {
	Provider  AccountType `json:"provider"`
	ID        string      `json:"id"`
	Identity  string      `json:"identity,omitempty"`
	DeletedAt time.Time   `json:"deleted_at"`
	Actor     string      `json:"actor"`
}

// deleteAccount hard-deletes an account's authority and per-account usage in a
// single bolt transaction (passport and usage share one db). The sanitized
// audit is written last so an audit failure rolls the whole delete back.
// Delete authorization is enforced by the caller (authorizeAccountDeletion).
func (p *PassportStore) deleteAccount(actor string, provider AccountType, id string) error {
	p.contributionMu.Lock()
	defer p.contributionMu.Unlock()
	return p.db.Update(func(tx *bbolt.Tx) error {
		resources := tx.Bucket([]byte(bucketAccountResources))
		resource, err := readAccountResource(resources, provider, id)
		if err != nil {
			return err
		}
		key := resourceKey(provider, id)

		grants, err := accountGrantsByResource(tx, provider, id, "")
		if err != nil {
			return err
		}
		if grantBucket := tx.Bucket([]byte(bucketAccountGrants)); grantBucket != nil {
			for _, grant := range grants {
				if err := grantBucket.Delete([]byte(grant.ID)); err != nil {
					return err
				}
			}
		}
		if index := tx.Bucket([]byte(bucketAccountGrantIndex)); index != nil && index.Bucket([]byte(key)) != nil {
			if err := index.DeleteBucket([]byte(key)); err != nil {
				return err
			}
		}

		if err := resources.Delete([]byte(key)); err != nil {
			return err
		}

		deletions, err := tx.CreateBucketIfNotExists([]byte(bucketAccountDeletions))
		if err != nil {
			return err
		}
		if err := putJSON(deletions, key, &accountDeletion{Provider: provider, ID: id, Identity: resource.Identity, DeletedAt: time.Now().UTC(), Actor: actor}); err != nil {
			return err
		}

		if err := purgeAccountUsage(tx, id); err != nil {
			return err
		}
		return p.audit(tx, actor, "account.deleted", id, string(provider))
	})
}

// purgeAccountUsage removes every per-account usage artifact while preserving
// user-level aggregates. Keys are collected before deletion so iteration is not
// mutated mid-scan.
func purgeAccountUsage(tx *bbolt.Tx, id string) error {
	if bucket := tx.Bucket([]byte(bucketUsageRequests)); bucket != nil {
		prefix := []byte(safeID(id) + "|")
		var doomed [][]byte
		cursor := bucket.Cursor()
		for k, _ := cursor.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, _ = cursor.Next() {
			doomed = append(doomed, append([]byte(nil), k...))
		}
		for _, k := range doomed {
			if err := bucket.Delete(k); err != nil {
				return err
			}
		}
	}
	if bucket := tx.Bucket([]byte(bucketAccountUsage)); bucket != nil {
		if err := bucket.Delete([]byte(id)); err != nil {
			return err
		}
	}
	if bucket := tx.Bucket([]byte(bucketOriginWeeklyUsage)); bucket != nil {
		hash := hashAccountID(id)
		var doomed [][]byte
		if err := bucket.ForEach(func(k, _ []byte) error {
			parts := strings.Split(string(k), "|")
			if len(parts) >= 3 && parts[2] == hash {
				doomed = append(doomed, append([]byte(nil), k...))
			}
			return nil
		}); err != nil {
			return err
		}
		for _, k := range doomed {
			if err := bucket.Delete(k); err != nil {
				return err
			}
		}
	}
	if bucket := tx.Bucket([]byte(bucketAnalyticsOutbox)); bucket != nil {
		var doomed [][]byte
		if err := bucket.ForEach(func(k, v []byte) error {
			var fact AnalyticsFact
			if err := json.Unmarshal(v, &fact); err != nil {
				return err
			}
			if fact.AccountID == id {
				doomed = append(doomed, append([]byte(nil), k...))
			}
			return nil
		}); err != nil {
			return err
		}
		for _, k := range doomed {
			if err := bucket.Delete(k); err != nil {
				return err
			}
		}
	}
	return nil
}

func (h *proxyHandler) handleAccountDelete(w http.ResponseWriter, r *http.Request, id string) {
	noStore(w)
	if h.passport == nil {
		respondJSONError(w, http.StatusServiceUnavailable, "account authority unavailable")
		return
	}
	var target *Account
	if h.pool != nil {
		for _, a := range h.pool.allAccounts() {
			if a.ID == id {
				target = a
				break
			}
		}
	}
	if target == nil {
		respondJSONError(w, http.StatusNotFound, "account not found")
		return
	}
	if atomic.LoadInt64(&target.Inflight) != 0 {
		respondJSONError(w, http.StatusConflict, "account has in-flight requests; retry when idle")
		return
	}
	actor := providerContributionActor(r)
	if err := h.passport.deleteAccount(actor, target.Type, id); err != nil {
		respondPolicyError(w, err)
		return
	}

	partialPurge := false
	if target.File != "" {
		if err := os.Remove(target.File); err != nil && !os.IsNotExist(err) {
			log.Printf("delete account %s: credential file removal failed: %v", id, err)
			partialPurge = true
		}
	}

	h.pool.mu.Lock()
	accounts := make([]*Account, 0, len(h.pool.accounts))
	for _, a := range h.pool.accounts {
		if a != nil && a.ID == id {
			continue
		}
		accounts = append(accounts, a)
	}
	h.pool.accounts = accounts
	for conversation, pinned := range h.pool.convPin {
		if pinned == id {
			h.pool.unpinLocked(conversation)
		}
	}
	h.pool.mu.Unlock()

	if h.store != nil {
		h.store.lastRateLimitsMu.Lock()
		delete(h.store.lastRateLimits, id)
		h.store.lastRateLimitsMu.Unlock()
	}

	if h.duckAnalytics != nil {
		if err := h.duckAnalytics.DeleteAccountUsage(id); err != nil {
			log.Printf("delete account %s: analytics purge failed: %v", id, err)
			partialPurge = true
		}
	}

	respondJSON(w, map[string]any{"id": id, "status": "deleted", "partial_purge": partialPurge})
}

// DeleteAccountUsage removes an account's analytical events. It holds the write
// side of queryMu so it cannot interleave with the drain insert transaction.
func (a *DuckAnalytics) DeleteAccountUsage(accountID string) error {
	if a == nil || a.db == nil {
		return nil
	}
	a.queryMu.Lock()
	defer a.queryMu.Unlock()
	tx, err := a.db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback()
	if _, err := tx.Exec(`DELETE FROM usage_events WHERE account_id=?`, accountID); err != nil {
		return err
	}
	if _, err := tx.Exec(`DELETE FROM legacy_cost_days WHERE account_id=?`, accountID); err != nil {
		return err
	}
	return tx.Commit()
}
