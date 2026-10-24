package main

import (
	"crypto/subtle"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strings"
	"time"

	"codex-pool-proxy/internal/runnerproto"
	"github.com/google/uuid"
	"go.etcd.io/bbolt"
)

const bucketRunnerEnrollments = "runner_enrollments"
const bucketRunners = "runners"
const bucketRunnerSessions = "runner_sessions"
const bucketSessionApprovals = "session_approvals"

type runnerEnrollment struct {
	PrincipalID string    `json:"principal_id"`
	ExpiresAt   time.Time `json:"expires_at"`
	RunnerID    string    `json:"runner_id,omitempty"`
}

type runnerRecord struct {
	Runner      runnerproto.Runner `json:"runner"`
	PrincipalID string             `json:"principal_id"`
	Digest      string             `json:"credential_digest"`
}

type connectedRunner struct {
	id, principal string
	hello         runnerproto.Hello
	lastSeen      time.Time
	out           chan runnerproto.Message
	stop          func()
}

func tokenDigest(token string) string {
	digest := hashToken(token)
	return hex.EncodeToString(digest[:])
}

func (b *sessionBroker) enrollment(principal string) (*runnerproto.Enrollment, error) {
	return b.newEnrollment(principal, "")
}

func (b *sessionBroker) newEnrollment(principal, runnerID string) (*runnerproto.Enrollment, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	nonce, err := secureToken(32)
	if err != nil {
		return nil, err
	}
	expires := b.now().Add(10 * time.Minute)
	err = b.store.db.Update(func(tx *bbolt.Tx) error {
		if runnerID != "" {
			var record runnerRecord
			raw := tx.Bucket([]byte(bucketRunners)).Get([]byte(runnerID))
			if raw == nil {
				return sessionError(404, "runner not found")
			}
			if err := json.Unmarshal(raw, &record); err != nil {
				return err
			}
			if record.PrincipalID != principal || record.Runner.Revoked {
				return sessionError(404, "runner not found")
			}
			for _, s := range b.sessions {
				if s.RunnerID == runnerID && s.State != "closed" {
					return sessionError(409, "close runner sessions before rotating its credential")
				}
			}
		}
		bucket := tx.Bucket([]byte(bucketRunnerEnrollments))
		count := 0
		cursor := bucket.Cursor()
		for k, v := cursor.First(); k != nil; k, v = cursor.Next() {
			var item runnerEnrollment
			if err := json.Unmarshal(v, &item); err != nil {
				return err
			}
			if !item.ExpiresAt.After(b.now()) {
				if err := cursor.Delete(); err != nil {
					return err
				}
				continue
			}
			if item.PrincipalID == principal {
				count++
			}
		}
		if count >= 8 {
			return sessionError(409, "too many pending enrollments")
		}
		if err := putJSON(bucket, tokenDigest(nonce), runnerEnrollment{PrincipalID: principal, ExpiresAt: expires, RunnerID: runnerID}); err != nil {
			return err
		}
		return b.store.audit(tx, principal, "runner.enrollment", "", "")
	})
	if err != nil {
		return nil, err
	}
	return &runnerproto.Enrollment{Nonce: nonce, ExpiresAt: expires, RunnerID: runnerID}, nil
}

func (b *sessionBroker) enroll(q runnerproto.EnrollRequest) (*runnerproto.EnrollResponse, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	if len(q.Nonce) != 43 || strings.TrimSpace(q.Name) == "" || len(q.Name) > 80 {
		return nil, sessionError(400, "invalid enrollment")
	}
	token, err := secureToken(32)
	if err != nil {
		return nil, err
	}
	id := uuid.NewString()
	credential := ""
	err = b.store.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket([]byte(bucketRunnerEnrollments))
		raw := bucket.Get([]byte(tokenDigest(q.Nonce)))
		if raw == nil {
			return sessionError(401, "invalid or expired enrollment")
		}
		var item runnerEnrollment
		if err := json.Unmarshal(raw, &item); err != nil {
			return err
		}
		if !item.ExpiresAt.After(b.now()) {
			return sessionError(401, "invalid or expired enrollment")
		}
		var principal Principal
		if err := json.Unmarshal(tx.Bucket([]byte(bucketPrincipals)).Get([]byte(item.PrincipalID)), &principal); err != nil {
			return err
		}
		if !runnerPrincipalUsable(&principal, b.now()) {
			return sessionError(403, "account unavailable")
		}
		var rotating *runnerRecord
		if item.RunnerID != "" {
			var current runnerRecord
			if err := json.Unmarshal(tx.Bucket([]byte(bucketRunners)).Get([]byte(item.RunnerID)), &current); err != nil {
				return err
			}
			if current.PrincipalID != principal.ID || current.Runner.Revoked {
				return sessionError(403, "runner unavailable")
			}
			for _, s := range b.sessions {
				if s.RunnerID == item.RunnerID && s.State != "closed" {
					return sessionError(409, "close runner sessions before rotating its credential")
				}
			}
			id = item.RunnerID
			rotating = &current
		}
		credential = "pool-runner-" + id + "." + token
		count := 0
		if err := tx.Bucket([]byte(bucketRunners)).ForEach(func(_, v []byte) error {
			var r runnerRecord
			if err := json.Unmarshal(v, &r); err != nil {
				return err
			}
			if r.PrincipalID == principal.ID && !r.Runner.Revoked {
				count++
			}
			return nil
		}); err != nil {
			return err
		}
		if count >= 50 && rotating == nil {
			return sessionError(409, "runner limit reached")
		}
		r := runnerRecord{PrincipalID: principal.ID, Digest: tokenDigest(credential), Runner: runnerproto.Runner{
			ID: id, Name: strings.TrimSpace(q.Name), CreatedAt: b.now(), Agents: []string{}, Labels: []string{}, AllowedPaths: []string{},
		}}
		if rotating != nil {
			r.Runner = rotating.Runner
			r.Runner.Name = strings.TrimSpace(q.Name)
		}
		if err := putJSON(tx.Bucket([]byte(bucketRunners)), id, r); err != nil {
			return err
		}
		if err := bucket.Delete([]byte(tokenDigest(q.Nonce))); err != nil {
			return err
		}
		action := "runner.enrolled"
		if rotating != nil {
			action = "runner.rotated"
		}
		return b.store.audit(tx, principal.ID, action, id, "")
	})
	if err != nil {
		return nil, err
	}
	if r := b.runners[id]; r != nil {
		r.stop()
	}
	return &runnerproto.EnrollResponse{RunnerID: id, Token: credential}, nil
}

func runnerPrincipalUsable(principal *Principal, now time.Time) bool {
	return principal != nil && principal.Status == PrincipalActive &&
		(principal.Kind == PrincipalMember || principal.Kind == PrincipalOperator) &&
		(principal.ExpiresAt == nil || principal.ExpiresAt.After(now)) &&
		(principal.Kind != PrincipalOperator || principalIsUsableOperator(principal))
}

func (b *sessionBroker) runnerCredential(r *http.Request) (*runnerRecord, error) {
	if len(r.Header.Values("Authorization")) != 1 {
		return nil, sessionError(401, "runner authentication required")
	}
	header := r.Header.Get("Authorization")
	if !strings.HasPrefix(header, "Bearer ") {
		return nil, sessionError(401, "runner bearer required")
	}
	token := strings.TrimPrefix(header, "Bearer ")
	if len(token) != len("pool-runner-")+36+1+43 || !strings.HasPrefix(token, "pool-runner-") {
		return nil, sessionError(401, "invalid runner credential")
	}
	id := token[len("pool-runner-") : len("pool-runner-")+36]
	var runner runnerRecord
	err := b.store.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket([]byte(bucketRunners)).Get([]byte(id))
		if raw == nil {
			return sessionError(401, "invalid runner credential")
		}
		if err := json.Unmarshal(raw, &runner); err != nil {
			return err
		}
		if runner.Runner.Revoked || subtle.ConstantTimeCompare([]byte(runner.Digest), []byte(tokenDigest(token))) != 1 {
			return sessionError(401, "invalid runner credential")
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	if !runnerPrincipalUsable(b.store.principal(runner.PrincipalID), b.now()) {
		return nil, sessionError(401, "account unavailable")
	}
	return &runner, nil
}

func (b *sessionBroker) listRunners(principal string) ([]runnerproto.Runner, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := []runnerproto.Runner{}
	err := b.store.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket([]byte(bucketRunners)).ForEach(func(_, value []byte) error {
			var r runnerRecord
			if err := json.Unmarshal(value, &r); err != nil {
				return err
			}
			if r.PrincipalID == principal {
				r.Runner.Online = !r.Runner.Revoked && b.runners[r.Runner.ID] != nil
				out = append(out, r.Runner)
			}
			return nil
		})
	})
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	return out, err
}

func (b *sessionBroker) revokeRunner(principal, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	err := b.store.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket([]byte(bucketRunners))
		raw := bucket.Get([]byte(id))
		if raw == nil {
			return sessionError(404, "runner not found")
		}
		var r runnerRecord
		if err := json.Unmarshal(raw, &r); err != nil {
			return err
		}
		if r.PrincipalID != principal {
			return sessionError(404, "runner not found")
		}
		r.Runner.Revoked = true
		if err := putJSON(bucket, id, r); err != nil {
			return err
		}
		return b.store.audit(tx, principal, "runner.revoked", id, "")
	})
	if err != nil {
		return err
	}
	if runner := b.runners[id]; runner != nil {
		runner.stop()
	}
	for _, s := range b.sessions {
		if s.RunnerID == id && s.State != "closed" {
			if err := b.closeLocked(s, "runner_revoked"); err != nil {
				return err
			}
		}
	}
	return nil
}

func validHello(h *runnerproto.Hello) error {
	if h == nil || len(h.Agents) == 0 || len(h.Agents) > 2 || h.OS == "" || h.Arch == "" || len(h.Labels) > 32 || len(h.AllowedPaths) > 64 {
		return errors.New("invalid runner hello")
	}
	seen := map[string]bool{}
	for _, agent := range h.Agents {
		if (agent != "codex" && agent != "claude") || seen[agent] {
			return errors.New("invalid runner agents")
		}
		seen[agent] = true
	}
	for _, value := range append(append([]string{h.OS, h.Arch}, h.Labels...), h.AllowedPaths...) {
		if len(value) == 0 || len(value) > 4096 || strings.ContainsAny(value, "\x00\r\n") {
			return errors.New("invalid runner capability")
		}
	}
	return nil
}
