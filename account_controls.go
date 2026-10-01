package main

import (
	"encoding/json"
	"errors"
	"io"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"go.etcd.io/bbolt"
)

type accountMode uint8

const (
	accountEnabled accountMode = iota
	accountDisabled
	accountMaintenance
	accountDraining
)

func (m accountMode) String() string {
	switch m {
	case accountEnabled:
		return "enabled"
	case accountDisabled:
		return "disabled"
	case accountMaintenance:
		return "maintenance"
	case accountDraining:
		return "draining"
	default:
		return "invalid"
	}
}

func (m accountMode) MarshalJSON() ([]byte, error) {
	if m > accountDraining {
		return nil, errors.New("invalid account state")
	}
	return json.Marshal(m.String())
}

func (m *accountMode) UnmarshalJSON(data []byte) error {
	var state string
	if err := json.Unmarshal(data, &state); err != nil {
		return err
	}
	for _, candidate := range []accountMode{accountEnabled, accountDisabled, accountMaintenance, accountDraining} {
		if state == candidate.String() {
			*m = candidate
			return nil
		}
	}
	return errors.New("invalid account state")
}

type accountControls struct {
	State         accountMode `json:"state"`
	MaxConcurrent int         `json:"max_concurrent"`
}

func (c *accountControls) UnmarshalJSON(data []byte) error {
	var q struct {
		State         *accountMode `json:"state"`
		MaxConcurrent *int         `json:"max_concurrent"`
	}
	d := json.NewDecoder(strings.NewReader(string(data)))
	d.DisallowUnknownFields()
	if err := d.Decode(&q); err != nil {
		return err
	}
	if q.State == nil || q.MaxConcurrent == nil {
		return errors.New("state and max_concurrent are required")
	}
	c.State, c.MaxConcurrent = *q.State, *q.MaxConcurrent
	return c.validate()
}

func (c accountControls) validate() error {
	if c.State > accountDraining || c.MaxConcurrent < 0 || c.MaxConcurrent > 10000 {
		return &policyError{Status: 400, Code: "account_controls_invalid", Message: "invalid state or concurrency limit (0–10000)"}
	}
	return nil
}

func accountControlError(code string, status int) error {
	return &policyError{Status: status, Code: code, Message: strings.ReplaceAll(code, "_", " ")}
}

func (p *PassportStore) updateAccountControls(actor string, provider AccountType, id string, revision uint64, controls accountControls, reason string) error {
	if err := controls.validate(); err != nil {
		return err
	}
	reason = strings.TrimSpace(reason)
	if reason == "" || len(reason) > 240 {
		return accountControlError("account_reason_required", 400)
	}
	p.accountMu.Lock()
	defer p.accountMu.Unlock()
	if err := p.authorizeAccount(actor, &Account{Type: provider, ID: id}, "manage"); err != nil {
		return accountControlError("account_access_denied", 403)
	}
	return p.db.Update(func(tx *bbolt.Tx) error {
		b := tx.Bucket([]byte(bucketAccountResources))
		r, err := readAccountResource(b, provider, id)
		if err != nil {
			return err
		}
		if revision == 0 || r.Revision != revision {
			return accountControlError("account_revision_conflict", 409)
		}
		r.Controls = controls
		r.Revision++
		if err := putJSON(b, resourceKey(provider, id), r); err != nil {
			return err
		}
		return p.audit(tx, actor, "account.controls_updated", id, string(provider)+" "+controls.State.String()+" "+reason)
	})
}

func (p *PassportStore) controlAdmission(provider AccountType, id string, pinned bool, acquire bool) (func(), error) {
	p.accountMu.Lock()
	defer p.accountMu.Unlock()
	return p.controlAdmissionLocked(provider, id, pinned, acquire)
}

func (p *PassportStore) controlAdmissionLocked(provider AccountType, id string, pinned bool, acquire bool) (func(), error) {
	key := resourceKey(provider, id)
	err := p.db.View(func(tx *bbolt.Tx) error {
		r, err := readAccountResource(tx.Bucket([]byte(bucketAccountResources)), provider, id)
		if err != nil {
			return err
		}
		if r.WithdrawnAt != nil || r.Status != "active" {
			return accountControlError("account_access_denied", 403)
		}
		switch r.Controls.State {
		case accountDisabled, accountMaintenance:
			return accountControlError("account_unavailable", 503)
		case accountDraining:
			if !pinned {
				return accountControlError("account_draining", 503)
			}
		}
		if r.Controls.MaxConcurrent > 0 && p.accountInflight[key] >= r.Controls.MaxConcurrent {
			return accountControlError("account_concurrency_exceeded", 429)
		}
		return nil
	})
	if err != nil || !acquire {
		return nil, err
	}
	p.accountInflight[key]++
	var once sync.Once
	return func() {
		once.Do(func() {
			p.accountMu.Lock()
			defer p.accountMu.Unlock()
			p.accountInflight[key]--
			if p.accountInflight[key] == 0 {
				delete(p.accountInflight, key)
			}
		})
	}, nil
}

func (p *poolState) isAccountPinned(identity, conversation string, a *Account) bool {
	if conversation == "" || a == nil {
		return false
	}
	p.mu.RLock()
	defer p.mu.RUnlock()
	owner := p.convOwner[conversation]
	return p.convPin[conversation] == a.ID && (owner == "" || owner == identity) && time.Since(p.convUpdatedAt[conversation]) <= conversationPinTTL
}

func (p *poolState) controlExclusions(identity, conversation string, excluded map[string]bool) map[string]bool {
	if p.accountAuthority == nil {
		return excluded
	}
	result := make(map[string]bool, len(excluded))
	for id, value := range excluded {
		result[id] = value
	}
	for _, a := range p.allAccounts() {
		pinned := p.isAccountPinned(identity, conversation, a)
		if _, err := p.accountAuthority.controlAdmission(a.Type, a.ID, pinned, false); err != nil {
			result[a.ID] = true
		}
	}
	return result
}

func (h *proxyHandler) acquireAccountSlot(identity, conversation string, a *Account) (func(), error) {
	if h.pool == nil || h.pool.accountAuthority == nil {
		return func() {}, nil
	}
	pinned := h.pool.isAccountPinned(identity, conversation, a)
	p := h.pool.accountAuthority
	p.accountMu.Lock()
	defer p.accountMu.Unlock()
	if err := h.checkAccountUse(identity, a); err != nil {
		return nil, err
	}
	return p.controlAdmissionLocked(a.Type, a.ID, pinned, true)
}

type accountLeaseBody struct {
	io.ReadCloser
	release func()
}

func (h *proxyHandler) prepareWebSocketAccountChange(identity, conversation string, current **Account, release *func(), next *Account) (func(bool), error) {
	if next == nil {
		return nil, accountControlError("account_unavailable", 503)
	}
	previous := *current
	if previous == next {
		return func(bool) {}, nil
	}
	if err := h.checkWebSocketGrant(identity, next); err != nil {
		return nil, err
	}
	nextRelease, err := h.acquireAccountSlot(identity, conversation, next)
	if err != nil {
		return nil, err
	}
	var once sync.Once
	return func(commit bool) {
		once.Do(func() {
			if !commit {
				nextRelease()
				return
			}
			(*release)()
			*release = nextRelease
			atomic.AddInt64(&next.Inflight, 1)
			atomic.AddInt64(&previous.Inflight, -1)
			*current = next
		})
	}, nil
}

func (b *accountLeaseBody) Close() error {
	err := b.ReadCloser.Close()
	b.release()
	return err
}
