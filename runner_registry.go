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
	"go.etcd.io/bbolt"
)

const bucketRunners = "runners"
const bucketRunnerSessions = "runner_sessions"
const bucketSessionApprovals = "session_approvals"

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
	b.signalCatalogLocked()
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
	if !runnerproto.ValidCommandKeyID(h.CommandKeyID) {
		return errors.New("invalid runner command key")
	}
	if err := runnerproto.ValidateAgentModels(h.Models); err != nil {
		return err
	}
	if h.CatalogRevision != "" && (!h.ModelsKnown || h.CatalogRevision != runnerproto.ModelsRevision(h.Models)) {
		return errors.New("invalid runner catalog revision")
	}
	for _, model := range h.Models {
		if !h.ModelsKnown || !supportsAgent(h.Agents, model.Agent) {
			return errors.New("model for an unavailable runner agent")
		}
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
