package main

import (
	"crypto/subtle"
	"encoding/json"
	"net/http"
	"strings"
	"time"

	"codex-pool-proxy/internal/runnerproto"
	"github.com/google/uuid"
	"go.etcd.io/bbolt"
)

const bucketRunnerPairings = "runner_pairings"
const runnerPairingTTL = 5 * time.Minute

type runnerPairing struct {
	runnerproto.PairingReview
	SecretDigest     string    `json:"secret_digest"`
	CodeDigest       string    `json:"code_digest"`
	PrincipalID      string    `json:"principal_id,omitempty"`
	Decision         string    `json:"decision,omitempty"`
	CredentialDigest string    `json:"credential_digest,omitempty"`
	LastPoll         time.Time `json:"last_poll"`
	Attempts         int       `json:"attempts"`
	Source           string    `json:"source"`
}

func (b *sessionBroker) beginPairing(q runnerproto.PairingRequest, rotating *runnerRecord, source string) (*runnerproto.PairingStart, error) {
	q.Name = strings.TrimSpace(q.Name)
	if q.Name == "" || len(q.Name) > 80 || len(q.OS) == 0 || len(q.OS) > 32 || len(q.Arch) == 0 || len(q.Arch) > 32 || strings.ContainsAny(q.Name+q.OS+q.Arch, "\x00\r\n") {
		return nil, sessionError(400, "invalid machine details")
	}
	id, err := secureToken(32)
	if err != nil {
		return nil, err
	}
	secret, err := secureToken(32)
	if err != nil {
		return nil, err
	}
	code, err := secureToken(6)
	if err != nil {
		return nil, err
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	p := runnerPairing{PairingReview: runnerproto.PairingReview{ID: id, Name: q.Name, OS: q.OS, Arch: q.Arch, ExpiresAt: b.now().Add(runnerPairingTTL)}, SecretDigest: tokenDigest(secret), CodeDigest: tokenDigest(code), Source: tokenDigest(source)}
	if rotating != nil {
		p.RunnerID = rotating.Runner.ID
		p.PrincipalID = rotating.PrincipalID
		p.CredentialDigest = rotating.Digest
	}
	err = b.store.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket([]byte(bucketRunnerPairings))
		count, sourceCount := 0, 0
		c := bucket.Cursor()
		for k, v := c.First(); k != nil; k, v = c.Next() {
			var item runnerPairing
			if err := json.Unmarshal(v, &item); err != nil {
				return err
			}
			if !item.ExpiresAt.After(b.now()) {
				if err := c.Delete(); err != nil {
					return err
				}
				continue
			}
			count++
			if item.Source == p.Source {
				sourceCount++
			}
		}
		if count >= 1024 || sourceCount >= 8 {
			return sessionError(429, "too many pairing requests; wait for expiration")
		}
		return putJSON(bucket, id, p)
	})
	if err != nil {
		return nil, err
	}
	return &runnerproto.PairingStart{ID: id, DeviceSecret: secret, UserCode: code, ExpiresAt: p.ExpiresAt, Interval: 3}, nil
}

func (b *sessionBroker) pairingLocked(tx *bbolt.Tx, id string) (*runnerPairing, error) {
	if len(id) != 43 {
		return nil, sessionError(404, "pairing unavailable")
	}
	var p runnerPairing
	raw := tx.Bucket([]byte(bucketRunnerPairings)).Get([]byte(id))
	if raw == nil {
		return nil, sessionError(404, "pairing unavailable")
	}
	if err := json.Unmarshal(raw, &p); err != nil {
		return nil, err
	}
	if !p.ExpiresAt.After(b.now()) {
		return nil, sessionError(410, "pairing expired")
	}
	return &p, nil
}

func (b *sessionBroker) reviewPairing(principal, id string) (*runnerproto.PairingReview, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	var review runnerproto.PairingReview
	err := b.store.db.View(func(tx *bbolt.Tx) error {
		p, err := b.pairingLocked(tx, id)
		if err != nil {
			return err
		}
		if p.Decision != "" || (p.PrincipalID != "" && p.PrincipalID != principal) {
			return sessionError(409, "pairing unavailable")
		}
		review = p.PairingReview
		return nil
	})
	return &review, err
}

func (b *sessionBroker) decidePairing(principal, id string, q runnerproto.PairingDecision) error {
	if q.Decision != "approve" && q.Decision != "deny" {
		return sessionError(400, "invalid pairing decision")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var fault error
	err := b.store.db.Update(func(tx *bbolt.Tx) error {
		p, err := b.pairingLocked(tx, id)
		if err != nil {
			return err
		}
		if p.Decision != "" || (p.PrincipalID != "" && p.PrincipalID != principal) {
			return sessionError(409, "pairing unavailable")
		}
		var owner Principal
		if json.Unmarshal(tx.Bucket([]byte(bucketPrincipals)).Get([]byte(principal)), &owner) != nil || !runnerPrincipalUsable(&owner, b.now()) {
			return sessionError(403, "account unavailable")
		}
		if q.Decision == "approve" && subtle.ConstantTimeCompare([]byte(tokenDigest(q.UserCode)), []byte(p.CodeDigest)) != 1 {
			p.Attempts++
			if p.Attempts >= 5 {
				p.Decision = "deny"
			}
			fault = sessionError(403, "machine code does not match")
		} else {
			p.PrincipalID = principal
			p.Decision = q.Decision
			if p.RunnerID != "" {
				if _, err := b.rotationRecord(tx, p); err != nil {
					return err
				}
			}
			if err := b.store.audit(tx, principal, "runner.pairing."+q.Decision, p.RunnerID, ""); err != nil {
				return err
			}
		}
		return putJSON(tx.Bucket([]byte(bucketRunnerPairings)), id, p)
	})
	if err != nil {
		return err
	}
	return fault
}

func (b *sessionBroker) rotationRecord(tx *bbolt.Tx, p *runnerPairing) (*runnerRecord, error) {
	var r runnerRecord
	if json.Unmarshal(tx.Bucket([]byte(bucketRunners)).Get([]byte(p.RunnerID)), &r) != nil || r.PrincipalID != p.PrincipalID || r.Runner.Revoked || r.Digest != p.CredentialDigest {
		return nil, sessionError(403, "runner credential changed or revoked")
	}
	for _, s := range b.sessions {
		if s.RunnerID == p.RunnerID && s.State != "closed" {
			return nil, sessionError(409, "close runner sessions before rotating its credential")
		}
	}
	return &r, nil
}

func (b *sessionBroker) completePairing(id, secret string) (*runnerproto.RunnerCredential, error) {
	if len(secret) != 43 {
		return nil, sessionError(401, "device authentication required")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	var result *runnerproto.RunnerCredential
	var fault error
	err := b.store.db.Update(func(tx *bbolt.Tx) error {
		p, err := b.pairingLocked(tx, id)
		if err != nil {
			return err
		}
		if subtle.ConstantTimeCompare([]byte(tokenDigest(secret)), []byte(p.SecretDigest)) != 1 {
			return sessionError(401, "invalid device secret")
		}
		if p.Decision == "consumed" {
			return sessionError(409, "pairing already consumed")
		}
		if !p.LastPoll.IsZero() && b.now().Sub(p.LastPoll) < 3*time.Second {
			return sessionError(429, "polling too frequently")
		}
		p.LastPoll = b.now()
		if p.Decision == "deny" {
			return sessionError(403, "pairing denied")
		}
		if p.Decision == "" {
			fault = sessionError(202, "authorization pending")
			return putJSON(tx.Bucket([]byte(bucketRunnerPairings)), id, p)
		}
		var principal Principal
		if json.Unmarshal(tx.Bucket([]byte(bucketPrincipals)).Get([]byte(p.PrincipalID)), &principal) != nil || !runnerPrincipalUsable(&principal, b.now()) {
			return sessionError(403, "account unavailable")
		}
		id := uuid.NewString()
		r := runnerRecord{PrincipalID: p.PrincipalID, Runner: runnerproto.Runner{ID: id, Name: p.Name, OS: p.OS, Arch: p.Arch, CreatedAt: b.now(), Agents: []string{}, Labels: []string{}, AllowedPaths: []string{}}}
		if p.RunnerID != "" {
			old, err := b.rotationRecord(tx, p)
			if err != nil {
				return err
			}
			r = *old
			id = old.Runner.ID
		} else {
			count := 0
			err := tx.Bucket([]byte(bucketRunners)).ForEach(func(_, v []byte) error {
				var item runnerRecord
				if err := json.Unmarshal(v, &item); err != nil {
					return err
				}
				if item.PrincipalID == p.PrincipalID && !item.Runner.Revoked {
					count++
				}
				return nil
			})
			if err != nil {
				return err
			}
			if count >= 50 {
				return sessionError(409, "runner limit reached")
			}
		}
		secret, err := secureToken(32)
		if err != nil {
			return err
		}
		token := "pool-runner-" + id + "." + secret
		r.Digest = tokenDigest(token)
		if err := putJSON(tx.Bucket([]byte(bucketRunners)), id, r); err != nil {
			return err
		}
		p.Decision = "consumed"
		if err := putJSON(tx.Bucket([]byte(bucketRunnerPairings)), p.ID, p); err != nil {
			return err
		}
		action := "runner.enrolled"
		if p.RunnerID != "" {
			action = "runner.rotated"
		}
		if err := b.store.audit(tx, p.PrincipalID, action, id, ""); err != nil {
			return err
		}
		result = &runnerproto.RunnerCredential{RunnerID: id, Token: token}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if fault != nil {
		return nil, fault
	}
	if connected := b.runners[result.RunnerID]; connected != nil {
		connected.stop()
	}
	return result, nil
}

func (h *proxyHandler) handleRunnerPairing(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	if r.Method != http.MethodPost {
		w.Header().Set("Allow", "POST")
		respondJSONError(w, 405, "method not allowed")
		return
	}
	b, err := h.sessionBroker()
	if err != nil {
		sessionHTTPError(w, err)
		return
	}
	path := strings.TrimPrefix(r.URL.Path, "/api/runner/pairings")
	if path == "" {
		var q runnerproto.PairingRequest
		if err := decodeSessionJSON(w, r, &q); err != nil {
			sessionHTTPError(w, err)
			return
		}
		var rotating *runnerRecord
		if len(r.Header.Values("Authorization")) > 0 {
			rotating, err = b.runnerCredential(r)
			if err != nil {
				sessionHTTPError(w, err)
				return
			}
		}
		ip, err := requestClientIP(r)
		if err != nil {
			respondJSONError(w, 400, "invalid client address")
			return
		}
		result, err := b.beginPairing(q, rotating, ip)
		if err != nil {
			sessionHTTPError(w, err)
			return
		}
		respondJSON(w, result)
		return
	}
	id := strings.TrimPrefix(path, "/")
	if strings.Contains(id, "/") {
		http.NotFound(w, r)
		return
	}
	if len(r.Header.Values("Authorization")) != 1 || !strings.HasPrefix(r.Header.Get("Authorization"), "Bearer ") {
		respondJSONError(w, 401, "device authentication required")
		return
	}
	result, err := b.completePairing(id, strings.TrimPrefix(r.Header.Get("Authorization"), "Bearer "))
	if err != nil {
		sessionHTTPError(w, err)
		return
	}
	respondJSON(w, result)
}

func (h *proxyHandler) handleAppRunnerPairing(w http.ResponseWriter, r *http.Request) {
	noStore(w)
	principal, _ := h.authenticateAppRequest(r)
	if principal == nil {
		respondJSONError(w, 401, "app authentication required")
		return
	}
	b, err := h.sessionBroker()
	if err != nil {
		sessionHTTPError(w, err)
		return
	}
	id := strings.TrimPrefix(r.URL.Path, "/api/me/runner-pairings/")
	switch r.Method {
	case http.MethodGet:
		result, err := b.reviewPairing(principal.ID, id)
		if err != nil {
			sessionHTTPError(w, err)
			return
		}
		respondJSON(w, result)
	case http.MethodPost:
		var q runnerproto.PairingDecision
		if err := decodeSessionJSON(w, r, &q); err != nil {
			sessionHTTPError(w, err)
			return
		}
		if err := b.decidePairing(principal.ID, id, q); err != nil {
			sessionHTTPError(w, err)
			return
		}
		w.WriteHeader(http.StatusNoContent)
	default:
		w.Header().Set("Allow", "GET, POST")
		respondJSONError(w, 405, "method not allowed")
	}
}
