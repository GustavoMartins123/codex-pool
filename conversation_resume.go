package main

import (
	"errors"
	"time"

	"codex-pool-proxy/internal/runnerproto"
)

func (b *sessionBroker) resumeConversation(principal, client, id string) (*runnerproto.Session, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, err := b.conversation(principal, id)
	if err != nil {
		return nil, err
	}
	s, err := b.ownedLocked(principal, c.SessionID)
	if err != nil {
		return nil, err
	}
	if c.Deleting || !c.HistoryComplete || !c.ResumeAvailable || s.State != "closed" || s.TurnID != "" || len(s.Approvals) > 0 || s.Checkpoint == nil || !s.Checkpoint.Ready || !validAgentCheckpoint(s.Checkpoint) {
		return nil, sessionError(409, "conversation has no complete native checkpoint available for resume")
	}
	r := b.runners[s.RunnerID]
	if r == nil || r.principal != principal || !r.hello.PersistentSessions || !supportsAgent(r.hello.Agents, s.Agent) {
		return nil, sessionError(409, "original runner must be online with persistent session support")
	}
	if s.Workspace.Mode == "path" && !supportsAgent(r.hello.AllowedPaths, s.Workspace.Target) {
		return nil, sessionError(409, "original workspace is no longer allowed")
	}
	active := 0
	for _, session := range b.sessions {
		if session.PrincipalID == principal && session.State != "closed" {
			active++
		}
	}
	if active >= 50 {
		return nil, sessionError(409, "active session limit reached")
	}
	// Verify the complete transcript before reactivating execution. Reads are
	// bounded to pages; the archive prefix remains compressed and immutable.
	var after uint64
	for after < s.EventCursor {
		events, err := b.readEventsLocked(s, after, s.EventCursor, 256)
		if err != nil {
			return nil, err
		}
		if len(events) == 0 {
			return nil, errors.New("conversation restore made no progress")
		}
		after = events[len(events)-1].ID
	}
	next := copySession(s)
	next.ClientID = client
	next.LLMClientID = ""
	next.State = "pending_runner"
	next.Ready = false
	next.LeaseExpiresAt = time.Time{}
	next.LastSequence = 0
	next.ClosedAt = nil
	next.ClosedReason = ""
	next.ResumeState = ""
	next.ResumeRequested = true
	next.UpdatedAt = b.now()
	next.CancelRequested = false
	if err := b.saveLocked(next, "conversation.resumed", principal, newSessionEvent(runnerproto.EventState, runnerproto.StateEvent{State: "pending_runner", Source: "user_resume"})); err != nil {
		return nil, err
	}
	b.offerLocked(next)
	return &copySession(next).Session, nil
}
