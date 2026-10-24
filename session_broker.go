package main

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"sync"
	"time"

	"codex-pool-proxy/internal/runnerproto"
	"github.com/google/uuid"
	"go.etcd.io/bbolt"
)

const sessionLeaseTTL = 45 * time.Second
const sessionPendingTTL = 10 * time.Minute
const sessionApprovalTTL = 10 * time.Minute

type sessionFault struct {
	status  int
	message string
}

func (e *sessionFault) Error() string               { return e.message }
func sessionError(status int, message string) error { return &sessionFault{status, message} }

type sessionRecord struct {
	runnerproto.Session
	LLMClientID     string `json:"llm_client_id,omitempty"`
	QueuedInput     string `json:"queued_input,omitempty"`
	Ready           bool   `json:"ready"`
	ResumeState     string `json:"resume_state,omitempty"`
	CancelRequested bool   `json:"cancel_requested"`
	LastSequence    uint64 `json:"last_sequence"`
	EventCursor     uint64 `json:"event_cursor"`
	HistoryComplete bool   `json:"history_complete"`
}

type approvalRecord struct {
	runnerproto.Approval
	SessionID   string `json:"session_id"`
	PrincipalID string `json:"principal_id"`
	Decision    string `json:"decision,omitempty"`
}

type sessionEvent struct {
	ID   uint64          `json:"id"`
	Type string          `json:"type"`
	Data json.RawMessage `json:"data"`
	At   time.Time       `json:"at,omitzero"`
}
type sessionSubscription struct {
	events chan sessionEvent
	failed chan struct{}
}
type sessionStream struct {
	next        uint64
	history     []sessionEvent
	bytes       int
	subscribers map[*sessionSubscription]struct{}
}

type sessionBroker struct {
	mu       sync.Mutex
	store    *PassportStore
	now      func() time.Time
	sessions map[string]*sessionRecord
	runners  map[string]*connectedRunner
	streams  map[string]*sessionStream
	stopped  chan struct{}
	stopOnce sync.Once
}

func newSessionBroker(store *PassportStore) (*sessionBroker, error) {
	if store == nil {
		return nil, errors.New("accounts unavailable")
	}
	b := &sessionBroker{store: store, now: func() time.Time { return time.Now().UTC() }, sessions: map[string]*sessionRecord{}, runners: map[string]*connectedRunner{}, streams: map[string]*sessionStream{}, stopped: make(chan struct{})}
	err := store.db.Update(func(tx *bbolt.Tx) error {
		for _, name := range []string{bucketRunnerPairings, bucketRunners, bucketRunnerSessions, bucketSessionApprovals, bucketSessionEvents, bucketConversations, bucketConversationIndex, bucketConversationDue, bucketConversationSchema} {
			if _, err := tx.CreateBucketIfNotExists([]byte(name)); err != nil {
				return err
			}
		}
		if err := b.migrateConversationsTx(tx); err != nil {
			return err
		}
		return tx.Bucket([]byte(bucketRunnerSessions)).ForEach(func(_, raw []byte) error {
			var s sessionRecord
			if err := json.Unmarshal(raw, &s); err != nil {
				return err
			}
			if !validSessionState(s.State) {
				return fmt.Errorf("invalid persisted session state")
			}
			if s.ConversationID == "" || tx.Bucket([]byte(bucketConversations)).Get([]byte(s.ConversationID)) == nil {
				return errors.New("session conversation is missing")
			}
			if s.State != "closed" && s.State != "pending_runner" && s.State != "closing" && s.State != "orphaned" {
				s.ResumeState = s.State
				s.State = "orphaned"
				s.UpdatedAt = b.now()
				if err := b.recordEventsTx(tx, &s, []sessionEvent{newSessionEvent("state", map[string]any{"state": s.State})}); err != nil {
					return err
				}
			}
			b.sessions[s.ID] = &s
			return nil
		})
	})
	return b, err
}

func validSessionState(s string) bool {
	switch s {
	case "pending_runner", "claimed", "running", "waiting_input", "waiting_approval", "orphaned", "closing", "closed":
		return true
	}
	return false
}

func (h *proxyHandler) sessionBroker() (*sessionBroker, error) {
	h.runnerBrokerOnce.Do(func() { h.runnerBroker, h.runnerBrokerErr = newSessionBroker(h.passport) })
	return h.runnerBroker, h.runnerBrokerErr
}

func (h *proxyHandler) startSessionBroker(ctx context.Context) error {
	b, err := h.sessionBroker()
	if err != nil {
		return err
	}
	go func() {
		ticker := time.NewTicker(time.Second)
		defer ticker.Stop()
		defer b.stopOnce.Do(func() { close(b.stopped) })
		for {
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
				if err := b.sweep(); err != nil { // Persistence failures stop execution instead of acknowledging uncommitted state.
					b.mu.Lock()
					for _, r := range b.runners {
						r.stop()
					}
					b.mu.Unlock()
					b.stopOnce.Do(func() { close(b.stopped) })
					return
				}
			}
		}
	}()
	return nil
}

func copySession(s *sessionRecord) *sessionRecord {
	copy := *s
	copy.Approvals = append([]runnerproto.Approval{}, s.Approvals...)
	copy.Summaries = append([]runnerproto.TurnSummary{}, s.Summaries...)
	return &copy
}

func (b *sessionBroker) saveLocked(s *sessionRecord, action, actor string, events ...sessionEvent) error {
	if err := b.store.db.Update(func(tx *bbolt.Tx) error {
		if err := b.recordEventsTx(tx, s, events); err != nil {
			return err
		}
		if action != "" {
			return b.store.audit(tx, actor, action, s.ID, "")
		}
		return nil
	}); err != nil {
		return err
	}
	b.sessions[s.ID] = s
	b.broadcastEventsLocked(s.ID, events)
	return nil
}

func (b *sessionBroker) create(principal, client string, q runnerproto.CreateSession) (*runnerproto.Session, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	select {
	case <-b.stopped:
		return nil, errors.New("session broker unavailable")
	default:
	}
	count := 0
	for _, s := range b.sessions {
		if s.PrincipalID == principal && s.State != "closed" {
			count++
		}
	}
	if count >= 50 {
		return nil, sessionError(409, "active session limit reached")
	}
	now := b.now()
	s := &sessionRecord{HistoryComplete: true, Session: runnerproto.Session{ID: uuid.NewString(), PrincipalID: principal, ClientID: client, Agent: q.Agent, Model: q.Model, Workspace: q.Workspace,
		PermissionMode: q.PermissionMode, State: "pending_runner", CreatedAt: now, UpdatedAt: now, Approvals: []runnerproto.Approval{}, Summaries: []runnerproto.TurnSummary{}}}
	if q.Prompt != "" {
		s.TurnID = uuid.NewString()
		s.QueuedInput = q.Prompt
	}
	s.ConversationID = s.ID
	events := []sessionEvent{newSessionEvent("state", map[string]any{"state": s.State})}
	if q.Prompt != "" {
		events = append(events, newSessionEvent("turn.input", map[string]any{"turn_id": s.TurnID, "input": q.Prompt}))
	}
	if err := b.saveLocked(s, "session.created", principal, events...); err != nil {
		return nil, err
	}
	b.offerLocked(s)
	return &copySession(s).Session, nil
}

func (b *sessionBroker) offerLocked(s *sessionRecord) {
	for _, r := range b.runners {
		if r.principal == s.PrincipalID && supportsAgent(r.hello.Agents, s.Agent) && (s.RunnerID == "" || s.RunnerID == r.id) {
			m := runnerproto.Frame("session.pending")
			value := copySession(s).Session
			m.Session = &value
			m.SessionID = s.ID
			b.sendLocked(r, m)
		}
	}
}

func supportsAgent(agents []string, agent string) bool {
	for _, a := range agents {
		if a == agent {
			return true
		}
	}
	return false
}

func (b *sessionBroker) sendLocked(r *connectedRunner, m runnerproto.Message) bool {
	if r == nil {
		return false
	}
	select {
	case r.out <- m:
		return true
	default:
		r.stop()
		return false
	}
}

func (b *sessionBroker) session(principal, id string) (*runnerproto.Session, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, err := b.ownedLocked(principal, id)
	if err != nil {
		return nil, err
	}
	return &copySession(s).Session, nil
}
func (b *sessionBroker) ownedLocked(principal, id string) (*sessionRecord, error) {
	s := b.sessions[id]
	if s == nil || s.PrincipalID != principal {
		return nil, sessionError(404, "session not found")
	}
	return s, nil
}

func (b *sessionBroker) list(principal string) []runnerproto.Session {
	b.mu.Lock()
	defer b.mu.Unlock()
	out := []runnerproto.Session{}
	for _, s := range b.sessions {
		if s.PrincipalID == principal {
			out = append(out, copySession(s).Session)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	if len(out) > 200 {
		out = out[:200]
	}
	return out
}

func (b *sessionBroker) startTurn(principal, id, input string) (string, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, err := b.ownedLocked(principal, id)
	if err != nil {
		return "", err
	}
	if s.State != "waiting_input" || s.TurnID != "" || !s.LeaseExpiresAt.After(b.now()) || b.runners[s.RunnerID] == nil {
		return "", sessionError(409, "session is not ready for input")
	}
	next := copySession(s)
	next.TurnID = uuid.NewString()
	next.QueuedInput = input
	next.State = "running"
	next.UpdatedAt = b.now()
	next.CancelRequested = false
	if err := b.saveLocked(next, "session.turn", principal,
		newSessionEvent("state", map[string]any{"state": next.State}),
		newSessionEvent("turn.input", map[string]any{"turn_id": next.TurnID, "input": input})); err != nil {
		return "", err
	}
	b.dispatchTurnLocked(next)
	return next.TurnID, nil
}

func (b *sessionBroker) dispatchTurnLocked(s *sessionRecord) {
	if !s.Ready || s.QueuedInput == "" || s.State == "closing" || s.State == "closed" {
		return
	}
	m := runnerproto.Frame("turn.start")
	m.SessionID = s.ID
	m.TurnID = s.TurnID
	m.Input = s.QueuedInput
	b.sendLocked(b.runners[s.RunnerID], m)
}

func (b *sessionBroker) cancelTurn(principal, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, err := b.ownedLocked(principal, id)
	if err != nil {
		return err
	}
	if s.TurnID == "" || (s.State != "running" && s.State != "waiting_approval" && s.State != "orphaned") {
		return sessionError(409, "no cancellable turn")
	}
	next := copySession(s)
	next.CancelRequested = true
	next.UpdatedAt = b.now()
	if err := b.saveLocked(next, "session.turn_cancelled", principal); err != nil {
		return err
	}
	m := runnerproto.Frame("turn.cancel")
	m.SessionID = id
	m.TurnID = next.TurnID
	b.sendLocked(b.runners[next.RunnerID], m)
	return nil
}

func (b *sessionBroker) requestClose(principal, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	s, err := b.ownedLocked(principal, id)
	if err != nil {
		return err
	}
	if s.State == "closed" || s.State == "closing" {
		return nil
	}
	if s.RunnerID == "" {
		return b.closeLocked(s, "cancelled")
	}
	next := copySession(s)
	next.State = "closing"
	next.ClosedReason = "cancelled"
	next.UpdatedAt = b.now()
	if err := b.saveLocked(next, "session.closing", principal, newSessionEvent("state", map[string]any{"state": "closing"})); err != nil {
		return err
	}
	if err := b.revokeSessionClient(next); err != nil {
		return err
	}
	m := runnerproto.Frame("session.close")
	m.SessionID = id
	b.sendLocked(b.runners[next.RunnerID], m)
	return nil
}

func (b *sessionBroker) revokeSessionClient(s *sessionRecord) error {
	if s.LLMClientID == "" {
		return nil
	}
	return b.store.revokeClient(s.PrincipalID, s.PrincipalID, s.LLMClientID)
}

func (b *sessionBroker) closeLocked(s *sessionRecord, reason string) error {
	if s.State == "closed" {
		return nil
	}
	if err := b.revokeSessionClient(s); err != nil {
		return err
	}
	next := copySession(s)
	now := b.now()
	next.State = "closed"
	next.ClosedReason = reason
	next.ClosedAt = &now
	next.UpdatedAt = now
	next.QueuedInput = ""
	next.Approvals = []runnerproto.Approval{}
	events := []sessionEvent{newSessionEvent("state", map[string]any{"state": "closed"})}
	if reason == "agent_failed" || reason == "workspace_failed" || reason == "protocol_error" {
		events = append(events, newSessionEvent("error", map[string]any{"message": reason}))
	}
	events = append(events, newSessionEvent("session.closed", map[string]any{"reason": reason}))
	if err := b.saveLocked(next, "session.closed", s.PrincipalID, events...); err != nil {
		return err
	}
	m := runnerproto.Frame("session.close")
	m.SessionID = s.ID
	b.sendLocked(b.runners[s.RunnerID], m)
	return nil
}

func (b *sessionBroker) broadcastEventsLocked(id string, events []sessionEvent) {
	for _, event := range events {
		b.broadcastLocked(id, event)
	}
}

func (b *sessionBroker) broadcastLocked(id string, event sessionEvent) {
	stream := b.streams[id]
	if stream == nil {
		stream = &sessionStream{subscribers: map[*sessionSubscription]struct{}{}}
		b.streams[id] = stream
	}
	stream.next = event.ID
	stream.history = append(stream.history, event)
	stream.bytes += len(event.Data)
	for len(stream.history) > 512 || stream.bytes > 2<<20 {
		stream.bytes -= len(stream.history[0].Data)
		stream.history = stream.history[1:]
	}
	for sub := range stream.subscribers {
		select {
		case sub.events <- event:
		default:
			close(sub.failed)
			delete(stream.subscribers, sub)
		}
	}
}

func (b *sessionBroker) sweep() error {
	b.mu.Lock()
	defer b.mu.Unlock()
	now := b.now()
	for _, r := range b.runners {
		if now.Sub(r.lastSeen) > sessionLeaseTTL || !runnerPrincipalUsable(b.store.principal(r.principal), now) {
			r.stop()
		}
	}
	for id, stream := range b.streams {
		if s := b.sessions[id]; s != nil && s.State == "closed" && len(stream.subscribers) == 0 && now.Sub(s.UpdatedAt) > sessionPendingTTL {
			delete(b.streams, id)
		}
	}
	for _, s := range b.sessions {
		if s.State == "closed" {
			continue
		}
		identity := s.PrincipalID + "-c-" + s.ClientID
		principal, app, ownerOK := b.store.credentialState(identity)
		if ownerOK {
			_, _, ownerOK = b.store.authorizeIssuedCredential(identity, app.CreatedAt)
			ownerOK = ownerOK && !principal.PasswordChangedAt.After(app.CreatedAt)
		}
		if !ownerOK {
			if err := b.closeLocked(s, "app_credential_revoked"); err != nil {
				return err
			}
			continue
		}
		if s.State == "pending_runner" && !s.CreatedAt.Add(sessionPendingTTL).After(now) {
			if err := b.closeLocked(s, "no_runner"); err != nil {
				return err
			}
			continue
		}
		if s.State == "closing" && !s.UpdatedAt.Add(sessionLeaseTTL).After(now) {
			if err := b.closeLocked(s, s.ClosedReason); err != nil {
				return err
			}
			continue
		}
		if s.RunnerID != "" && s.State != "orphaned" && s.State != "closing" && !s.LeaseExpiresAt.After(now) {
			next := copySession(s)
			next.ResumeState = s.State
			next.State = "orphaned"
			next.UpdatedAt = now
			if err := b.saveLocked(next, "session.orphaned", s.PrincipalID,
				newSessionEvent("state", map[string]any{"state": "orphaned"}),
				newSessionEvent("runner.status", map[string]any{"status": "offline"})); err != nil {
				return err
			}
		}
		for _, approval := range s.Approvals {
			if !approval.ExpiresAt.After(now) {
				if err := b.resolveApprovalLocked(s.PrincipalID, s.ID, approval.ID, "deny", "timeout"); err != nil {
					return err
				}
			}
		}
	}
	return nil
}

func (b *sessionBroker) resolveApproval(principal, id, aid, decision string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.resolveApprovalLocked(principal, id, aid, decision, "user")
}
func (b *sessionBroker) resolveApprovalLocked(principal, id, aid, decision, source string) error {
	s, err := b.ownedLocked(principal, id)
	if err != nil {
		return err
	}
	if s.State == "closing" || s.State == "closed" {
		return sessionError(409, "session is closed")
	}
	if decision != "approve" && decision != "deny" {
		return sessionError(400, "invalid approval decision")
	}
	next := copySession(s)
	var record approvalRecord
	var events []sessionEvent
	err = b.store.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket([]byte(bucketSessionApprovals))
		raw := bucket.Get([]byte(aid))
		if raw == nil {
			return sessionError(404, "approval not found")
		}
		if err := json.Unmarshal(raw, &record); err != nil {
			return err
		}
		if record.SessionID != id || record.PrincipalID != principal {
			return sessionError(404, "approval not found")
		}
		if record.Decision != "" {
			if record.Decision == decision {
				return nil
			}
			return sessionError(409, "approval already resolved")
		}
		if record.TurnID != s.TurnID {
			return sessionError(409, "approval no longer belongs to the current turn")
		}
		if !record.ExpiresAt.After(b.now()) {
			decision = "deny"
			source = "timeout"
		}
		record.Decision = decision
		pending := []runnerproto.Approval{}
		for _, a := range next.Approvals {
			if a.ID != aid {
				pending = append(pending, a)
			}
		}
		next.Approvals = pending
		if next.State == "waiting_approval" && len(pending) == 0 {
			next.State = "running"
		}
		if next.State == "orphaned" && len(pending) == 0 && next.ResumeState == "waiting_approval" {
			next.ResumeState = "running"
		}
		next.UpdatedAt = b.now()
		if err := putJSON(bucket, aid, record); err != nil {
			return err
		}
		events = []sessionEvent{
			newSessionEvent("approval.resolved", map[string]any{"approval_id": aid, "decision": record.Decision, "source": source}),
			newSessionEvent("state", map[string]any{"state": next.State}),
		}
		if err := b.recordEventsTx(tx, next, events); err != nil {
			return err
		}
		return b.store.audit(tx, principal, "session.approval", id, decision)
	})
	if err != nil {
		return err
	}
	b.sessions[id] = next
	b.broadcastEventsLocked(id, events)
	m := runnerproto.Frame("approval.decision")
	m.SessionID = id
	m.TurnID = record.TurnID
	m.ApprovalID = aid
	m.Decision = record.Decision
	b.sendLocked(b.runners[s.RunnerID], m)
	return nil
}
