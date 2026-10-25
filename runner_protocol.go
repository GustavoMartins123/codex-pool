package main

import (
	"encoding/hex"
	"encoding/json"
	"errors"
	"strings"
	"time"

	"codex-pool-proxy/internal/runnerproto"
	"go.etcd.io/bbolt"
)

func (b *sessionBroker) claim(r *connectedRunner, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.runners[r.id] != r {
		return sessionError(409, "runner connection replaced")
	}
	s, err := b.ownedLocked(r.principal, id)
	if err != nil {
		return err
	}
	if !supportsAgent(r.hello.Agents, s.Agent) {
		return sessionError(409, "unsupported agent")
	}
	if s.State == "closed" {
		return sessionError(409, "session_closed")
	}
	if s.RunnerID != "" && s.RunnerID != r.id {
		return sessionError(409, "session belongs to another runner")
	}
	if s.State != "pending_runner" && s.State != "orphaned" && s.RunnerID != r.id {
		return sessionError(409, "session is not claimable")
	}
	if s.Workspace.Mode == "path" && !supportsAgent(r.hello.AllowedPaths, s.Workspace.Target) {
		return sessionError(409, "workspace path is not allowed")
	}
	next := copySession(s)
	next.RunnerID = r.id
	next.LeaseExpiresAt = b.now().Add(sessionLeaseTTL)
	next.UpdatedAt = b.now()
	if s.State == "pending_runner" {
		next.State = "claimed"
	} else if s.State == "orphaned" {
		next.State = s.ResumeState
		if !validSessionState(next.State) || next.State == "orphaned" {
			return errors.New("invalid resume state")
		}
	}
	nonce, err := b.claimCredentialLocked(next)
	if err != nil {
		return err
	}
	b.sessions[id] = next
	m := runnerproto.Frame("claim.ack")
	m.SessionID = id
	value := copySession(next).Session
	m.Session = &value
	m.Sequence = next.LastSequence
	if nonce != "" {
		m.CredentialPath = "/config/" + s.Agent + "/" + nonce
	}
	b.sendLocked(r, m)
	if next.State == "closing" {
		close := runnerproto.Frame("session.close")
		close.SessionID = id
		b.sendLocked(r, close)
		return nil
	}
	if next.CancelRequested {
		cancel := runnerproto.Frame("turn.cancel")
		cancel.SessionID = id
		cancel.TurnID = next.TurnID
		b.sendLocked(r, cancel)
	} else {
		b.dispatchTurnLocked(next)
	}
	return b.store.db.View(func(tx *bbolt.Tx) error {
		return tx.Bucket([]byte(bucketSessionApprovals)).ForEach(func(_, raw []byte) error {
			var a approvalRecord
			if err := json.Unmarshal(raw, &a); err != nil {
				return err
			}
			if a.SessionID == id && a.TurnID == next.TurnID && a.Decision != "" {
				m := runnerproto.Frame("approval.decision")
				m.SessionID = id
				m.TurnID = a.TurnID
				m.ApprovalID = a.ID
				m.Decision = a.Decision
				attachCommandSignature(&m, r, a.DecisionIssuedAt, a.DecisionSignatures)
				b.sendLocked(r, m)
			}
			return nil
		})
	})
}

func (b *sessionBroker) claimCredentialLocked(s *sessionRecord) (string, error) {
	p := b.store
	p.mu.Lock()
	defer p.mu.Unlock()
	principal := p.principals[s.PrincipalID]
	app := p.clients[s.ClientID]
	if !runnerPrincipalUsable(principal, b.now()) || app == nil || app.Status != "active" || app.Purpose != appClientPurpose || app.PrincipalID != s.PrincipalID || (app.ExpiresAt != nil && !app.ExpiresAt.After(b.now())) {
		return "", sessionError(403, "session owner unavailable")
	}
	if app.CreatedAt.Unix() < principal.CredentialsValidAfter.Unix() || app.CreatedAt.Unix() < app.ValidAfter.Unix() || principal.PasswordChangedAt.After(app.CreatedAt) {
		return "", sessionError(403, "session owner credential was invalidated")
	}
	var client *ClientCredential
	if s.LLMClientID != "" {
		current := p.clients[s.LLMClientID]
		if current == nil || current.Status != "active" || current.PrincipalID != s.PrincipalID {
			return "", sessionError(409, "session credential unavailable")
		}
		cp := *current
		client = &cp
	} else {
		count := 0
		for _, c := range p.clients {
			if c.PrincipalID == s.PrincipalID && c.Status == "active" {
				count++
			}
		}
		if count >= 20 {
			return "", sessionError(409, "client credential limit reached")
		}
		id, err := secureID(9)
		if err != nil {
			return "", err
		}
		download, err := secureToken(24)
		if err != nil {
			return "", err
		}
		sealed, err := p.seal("client", id, s.PrincipalID, download)
		if err != nil {
			return "", err
		}
		client = &ClientCredential{ID: id, PrincipalID: s.PrincipalID, Label: "runner " + s.ID, Purpose: "runner", Status: "active", DownloadDigest: tokenDigest(download), DownloadCiphertext: sealed, CreatedAt: b.now()}
		s.LLMClientID = id
	}
	expires := b.now().Add(time.Hour)
	if principal.ExpiresAt != nil && principal.ExpiresAt.Before(expires) {
		expires = *principal.ExpiresAt
	}
	client.ExpiresAt = &expires
	client.Policy = app.Policy
	client.Policy.Models.Allow = []string{s.Model}
	nonce, err := secureToken(24)
	if err != nil {
		return "", err
	}
	events := []sessionEvent{
		newSessionEvent("runner.status", map[string]any{"status": "reconnected"}),
		newSessionEvent("state", map[string]any{"state": s.State}),
	}
	err = p.db.Update(func(tx *bbolt.Tx) error {
		nonces := tx.Bucket([]byte(bucketConfigNonces))
		cursor := nonces.Cursor()
		for k, raw := cursor.First(); k != nil; k, raw = cursor.Next() {
			var previous configDownloadNonce
			if err := json.Unmarshal(raw, &previous); err != nil {
				return err
			}
			if previous.ClientID == client.ID || !previous.ExpiresAt.After(b.now()) {
				if err := cursor.Delete(); err != nil {
					return err
				}
			}
		}
		if err := putJSON(tx.Bucket([]byte(bucketClientCredentials)), client.ID, client); err != nil {
			return err
		}
		if err := putJSON(tx.Bucket([]byte(bucketConfigNonces)), tokenDigest(nonce), configDownloadNonce{ClientID: client.ID, PrincipalID: s.PrincipalID, ExpiresAt: b.now().Add(configDownloadNonceTTL)}); err != nil {
			return err
		}
		if err := b.recordEventsTx(tx, s, events); err != nil {
			return err
		}
		return p.audit(tx, s.PrincipalID, "session.claim", s.ID, s.RunnerID)
	})
	if err != nil {
		return "", err
	}
	p.clients[client.ID] = client
	b.broadcastEventsLocked(s.ID, events)
	return nonce, nil
}

func (b *sessionBroker) heartbeat(r *connectedRunner, ids []string) error {
	if len(ids) > 50 {
		return errors.New("heartbeat session limit exceeded")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.runners[r.id] != r {
		return errors.New("connection replaced")
	}
	now := b.now()
	updates := []*sessionRecord{}
	seen := map[string]bool{}
	for _, id := range ids {
		if seen[id] {
			return errors.New("duplicate heartbeat session")
		}
		seen[id] = true
		s := b.sessions[id]
		if s == nil || s.PrincipalID != r.principal || s.RunnerID != r.id {
			return errors.New("invalid heartbeat ownership")
		}
		if s.State == "closed" {
			m := runnerproto.Frame("session.close")
			m.SessionID = id
			b.sendLocked(r, m)
			continue
		}
		if s.State == "orphaned" || !s.LeaseExpiresAt.After(now) {
			m := runnerproto.Frame("claim.required")
			m.SessionID = id
			b.sendLocked(r, m)
			continue
		}
		next := copySession(s)
		next.LeaseExpiresAt = now.Add(sessionLeaseTTL)
		updates = append(updates, next)
	}
	p := b.store
	p.mu.Lock()
	defer p.mu.Unlock()
	clientUpdates := map[string]*ClientCredential{}
	err := p.db.Update(func(tx *bbolt.Tx) error {
		for _, s := range updates {
			if err := putJSON(tx.Bucket([]byte(bucketRunnerSessions)), s.ID, s); err != nil {
				return err
			}
			if s.LLMClientID != "" && s.State != "closing" {
				client := p.clients[s.LLMClientID]
				if client == nil || client.Status != "active" {
					return errors.New("session credential revoked")
				}
				cp := *client
				expiry := now.Add(time.Hour)
				pr := p.principals[s.PrincipalID]
				if pr == nil {
					return errors.New("principal unavailable")
				}
				if pr.ExpiresAt != nil && pr.ExpiresAt.Before(expiry) {
					expiry = *pr.ExpiresAt
				}
				cp.ExpiresAt = &expiry
				if err := putJSON(tx.Bucket([]byte(bucketClientCredentials)), cp.ID, &cp); err != nil {
					return err
				}
				clientUpdates[cp.ID] = &cp
			}
		}
		return nil
	})
	if err != nil {
		return err
	}
	for _, s := range updates {
		b.sessions[s.ID] = s
	}
	for id, c := range clientUpdates {
		p.clients[id] = c
	}
	r.lastSeen = now
	m := runnerproto.Frame("heartbeat.ack")
	b.sendLocked(r, m)
	return nil
}

func (b *sessionBroker) runnerEvent(r *connectedRunner, m runnerproto.Message) error {
	switch m.Type {
	case "workspace.status", "turn.started", "turn.event", "turn.finished", "approval.request", "approval.cancelled", "process.exit", "agent.checkpoint", "execution.error":
	default:
		return errors.New("unknown runner event")
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.runners[r.id] != r {
		return errors.New("connection replaced")
	}
	s := b.sessions[m.SessionID]
	if s == nil || s.PrincipalID != r.principal || s.RunnerID != r.id {
		return errors.New("invalid session ownership")
	}
	if m.Sequence == 0 {
		return errors.New("event sequence required")
	}
	ack := runnerproto.Frame("event.ack")
	ack.SessionID = s.ID
	ack.Sequence = s.LastSequence
	if m.Sequence <= s.LastSequence {
		b.sendLocked(r, ack)
		return nil
	}
	if m.Sequence != s.LastSequence+1 {
		return errors.New("event sequence gap")
	}
	if s.State == "closed" || (s.State == "closing" && m.Type != "process.exit") {
		next := copySession(s)
		next.LastSequence = m.Sequence
		if err := b.saveLocked(next, "", r.principal); err != nil {
			return err
		}
		ack.Sequence = m.Sequence
		b.sendLocked(r, ack)
		close := runnerproto.Frame("session.close")
		close.SessionID = s.ID
		b.sendLocked(r, close)
		return nil
	}
	if s.State == "orphaned" || !s.LeaseExpiresAt.After(b.now()) {
		return errors.New("session lease expired; claim required")
	}
	next := copySession(s)
	next.LastSequence = m.Sequence
	next.UpdatedAt = b.now()
	var event string
	var payload any
	var action string
	var approval *approvalRecord
	approvalExists := false
	switch m.Type {
	case "execution.error":
		if m.TurnID == "" || m.TurnID != s.TurnID || m.Content == "" || len(m.Content) > 8<<10 {
			return errors.New("invalid execution error")
		}
		event = "error"
		payload = map[string]any{"message": m.Content}
		action = "session.execution_error"
	case "agent.checkpoint":
		if !r.hello.PersistentSessions || s.State != "claimed" || s.Ready || !validAgentCheckpoint(m.Checkpoint) || m.Checkpoint.Ready {
			return errors.New("unexpected native checkpoint")
		}
		if s.ResumeRequested && (s.Checkpoint == nil || s.Checkpoint.NativeSessionID != m.Checkpoint.NativeSessionID || s.Checkpoint.NativeLogID != m.Checkpoint.NativeLogID) {
			return errors.New("native resume checkpoint identity changed")
		}
		checkpoint := *m.Checkpoint
		next.Checkpoint = &checkpoint
		action = "session.native_checkpoint"
	case "workspace.status":
		if m.Status == "failed" {
			if err := b.saveLocked(next, "", r.principal); err != nil {
				return err
			}
			return b.closeLocked(next, "workspace_failed")
		}
		if m.Status != "preparing" && m.Status != "ready" {
			return errors.New("invalid workspace status")
		}
		if m.Status == "ready" {
			next.Ready = true
			if next.TurnID == "" {
				next.State = "waiting_input"
			} else {
				next.State = "running"
			}
			action = "session.ready"
		}
	case "turn.started":
		if !s.Ready || m.TurnID == "" || m.TurnID != s.TurnID || s.QueuedInput == "" {
			return errors.New("unexpected turn start")
		}
		next.QueuedInput = ""
		next.QueuedIssuedAt, next.QueuedSignatures = 0, nil
		next.State = "running"
		event = "turn.started"
		payload = map[string]any{"turn_id": m.TurnID}
		action = "session.running"
	case "turn.event":
		if m.TurnID == "" || m.TurnID != s.TurnID || s.QueuedInput != "" || len(m.Content) > 64<<10 {
			return errors.New("unexpected turn delta")
		}
		if m.Kind != "text" && m.Kind != "thinking" && m.Kind != "tool_call" && m.Kind != "tool_result" {
			return errors.New("invalid delta kind")
		}
		event = "turn.delta"
		payload = map[string]any{"turn_id": m.TurnID, "kind": m.Kind, "content": m.Content}
	case "turn.finished":
		if m.TurnID == "" || m.TurnID != s.TurnID || s.QueuedInput != "" || m.Tokens < 0 || m.DurationMS < 0 {
			return errors.New("unexpected turn completion")
		}
		next.Turns++
		summary := runnerproto.TurnSummary{TurnID: m.TurnID, Tokens: m.Tokens, DurationMS: m.DurationMS, ExitCode: m.ExitCode}
		next.Summaries = append(next.Summaries, summary)
		if len(next.Summaries) > 100 {
			next.Summaries = next.Summaries[len(next.Summaries)-100:]
		}
		next.State = "waiting_input"
		next.TurnID = ""
		next.CancelRequested = false
		next.Approvals = []runnerproto.Approval{}
		event = "turn.finished"
		payload = summary
		action = "session.turn_finished"
	case "approval.request":
		if s.PermissionMode != "gated" || m.TurnID == "" || m.TurnID != s.TurnID || s.QueuedInput != "" || m.ApprovalID == "" || len(m.ApprovalID) > 128 || len(m.Command) == 0 || len(m.Command) > 32<<10 || len(m.Context) > 32<<10 || len(s.Approvals) >= 16 {
			return errors.New("unexpected approval request")
		}
		for _, a := range s.Approvals {
			if a.ID == m.ApprovalID {
				return errors.New("duplicate approval")
			}
		}
		a := runnerproto.Approval{ID: m.ApprovalID, Command: m.Command, Context: m.Context, TurnID: m.TurnID, ExpiresAt: b.now().Add(sessionApprovalTTL)}
		approval = &approvalRecord{Approval: a, SessionID: s.ID, PrincipalID: s.PrincipalID}
		next.Approvals = append(next.Approvals, a)
		next.State = "waiting_approval"
		event = "approval.requested"
		payload = a
		action = "session.approval_requested"
	case "approval.cancelled":
		if m.TurnID != s.TurnID || m.ApprovalID == "" {
			return errors.New("invalid approval cancellation")
		}
		if err := b.store.db.View(func(tx *bbolt.Tx) error {
			var record approvalRecord
			if err := json.Unmarshal(tx.Bucket([]byte(bucketSessionApprovals)).Get([]byte(m.ApprovalID)), &record); err != nil {
				return err
			}
			if record.SessionID != s.ID || record.TurnID != s.TurnID {
				return errors.New("invalid approval ownership")
			}
			record.Decision = "deny"
			approval = &record
			return nil
		}); err != nil {
			return err
		}
		pending := []runnerproto.Approval{}
		for _, a := range next.Approvals {
			if a.ID != m.ApprovalID {
				pending = append(pending, a)
			}
		}
		next.Approvals = pending
		if len(pending) == 0 {
			next.State = "running"
		}
		approvalExists = true
		action = "session.approval_cancelled"
		event = "approval.resolved"
		payload = map[string]any{"approval_id": m.ApprovalID, "decision": "deny"}
	case "process.exit":
		if m.Status == "checkpoint_failed" && next.Checkpoint != nil {
			next.Checkpoint.Ready = false
			next.Checkpoint.Digest = ""
		}
		if m.Checkpoint != nil {
			if !r.hello.PersistentSessions || !validAgentCheckpoint(m.Checkpoint) || !m.Checkpoint.Ready || s.Checkpoint == nil || m.Checkpoint.NativeSessionID != s.Checkpoint.NativeSessionID || m.Checkpoint.NativeLogID != s.Checkpoint.NativeLogID {
				return errors.New("invalid durable native checkpoint")
			}
			checkpoint := *m.Checkpoint
			next.Checkpoint = &checkpoint
		}
		if err := b.saveLocked(next, "", r.principal); err != nil {
			return err
		}
		reason := "process_exit"
		if s.State == "closing" {
			reason = s.ClosedReason
		}
		if m.ExitCode != 0 {
			reason = "agent_failed"
		}
		if m.Status == "checkpoint_failed" {
			reason = "checkpoint_failed"
		}
		if m.Status == runnerproto.ExitAutoPermissionsDisabled {
			reason = runnerproto.ExitAutoPermissionsDisabled
		}
		return b.closeLocked(next, reason)
	default:
		return errors.New("unknown runner event")
	}
	var events []sessionEvent
	if event != "" {
		events = append(events, newSessionEvent(event, payload))
	}
	if next.State != s.State {
		events = append(events, newSessionEvent("state", map[string]any{"state": next.State}))
	}
	if approval != nil {
		if err := b.store.db.Update(func(tx *bbolt.Tx) error {
			bucket := tx.Bucket([]byte(bucketSessionApprovals))
			if bucket.Get([]byte(approval.ID)) != nil && !approvalExists {
				return errors.New("approval id already used")
			}
			if err := putJSON(bucket, approval.ID, approval); err != nil {
				return err
			}
			if err := b.recordEventsTx(tx, next, events); err != nil {
				return err
			}
			return b.store.audit(tx, r.principal, action, s.ID, "")
		}); err != nil {
			return err
		}
		b.sessions[s.ID] = next
		b.broadcastEventsLocked(s.ID, events)
	} else if err := b.saveLocked(next, action, r.principal, events...); err != nil {
		return err
	}
	ack.Sequence = next.LastSequence
	b.sendLocked(r, ack)
	if m.Type == "workspace.status" && m.Status == "ready" {
		b.dispatchTurnLocked(next)
	}
	return nil
}

func validAgentCheckpoint(checkpoint *runnerproto.AgentCheckpoint) bool {
	if checkpoint == nil || checkpoint.Version != 1 {
		return false
	}
	if checkpoint.Ready {
		if len(checkpoint.Digest) != 64 {
			return false
		}
		if _, err := hex.DecodeString(checkpoint.Digest); err != nil {
			return false
		}
	} else if checkpoint.Digest != "" {
		return false
	}
	for _, id := range []string{checkpoint.NativeSessionID, checkpoint.NativeLogID} {
		if len(id) == 0 || len(id) > 128 {
			return false
		}
		for _, c := range id {
			if !((c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || (c >= '0' && c <= '9') || c == '-' || c == '_') {
				return false
			}
		}
	}
	return true
}

func (b *sessionBroker) releaseClaim(r *connectedRunner, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.runners[r.id] != r {
		return errors.New("connection replaced")
	}
	s, err := b.ownedLocked(r.principal, id)
	if err != nil {
		return err
	}
	if s.RunnerID != r.id || s.State != "claimed" || s.Ready {
		return sessionError(409, "only an unstarted claim can be released")
	}
	if err := b.revokeSessionClient(s); err != nil {
		return err
	}
	next := copySession(s)
	next.RunnerID = ""
	next.LLMClientID = ""
	next.State = "pending_runner"
	next.LastSequence = 0
	next.LeaseExpiresAt = time.Time{}
	next.UpdatedAt = b.now()
	if err := b.saveLocked(next, "session.claim_released", r.principal, newSessionEvent("state", map[string]any{"state": next.State})); err != nil {
		return err
	}
	b.offerLocked(next)
	return nil
}

func validateSessionRequest(q runnerproto.CreateSession) error {
	if q.Agent != "codex" && q.Agent != "claude" {
		return sessionError(400, "unsupported agent")
	}
	if q.PermissionMode != "auto" && q.PermissionMode != "gated" {
		return sessionError(400, "invalid permission mode")
	}
	if q.Model == "" || len(q.Model) > 256 || strings.TrimSpace(q.Model) != q.Model {
		return sessionError(400, "invalid model")
	}
	// Runners only run prompts signed for an existing session, so the first
	// prompt is sent as a signed turn once the runner is ready.
	if q.Prompt != "" {
		return sessionError(400, "send the first prompt as a signed turn")
	}
	if len(q.InitialInput) > runnerproto.MaxInputBytes || q.InitialInput != "" && strings.TrimSpace(q.InitialInput) == "" {
		return sessionError(400, "invalid initial input")
	}
	switch q.Workspace.Mode {
	case "ephemeral":
		if q.Workspace.Target != "" {
			return sessionError(400, "ephemeral workspace has no target")
		}
	case "path":
		if q.PermissionMode != "gated" || q.Workspace.Target == "" {
			return sessionError(400, "path workspace requires gated permissions and a target")
		}
	case "repo":
		if q.Workspace.Target == "" {
			return sessionError(400, "repository URL required")
		}
	default:
		return sessionError(400, "invalid workspace mode")
	}
	if len(q.Workspace.Target) > 4096 || strings.ContainsAny(q.Workspace.Target, "\x00\r\n") {
		return sessionError(400, "invalid workspace target")
	}
	return nil
}
