package main

import (
	"bytes"
	"encoding/json"
	"errors"

	"codex-pool-proxy/internal/runnerproto"
	"go.etcd.io/bbolt"
)

func (b *sessionBroker) deleteConversation(principal, id string) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	c, err := b.conversation(principal, id)
	if err != nil {
		return err
	}
	s, err := b.ownedLocked(principal, c.SessionID)
	if err != nil {
		return err
	}
	if s.State != "closed" {
		return sessionError(409, "close the execution before deleting its conversation")
	}
	if s.RunnerID == "" {
		return b.eraseConversationLocked(c, s)
	}
	r := b.runners[s.RunnerID]
	if r == nil || r.principal != principal || !r.hello.PersistentSessions {
		return sessionError(409, "original runner must be online to delete the persisted workspace and checkpoint")
	}
	next := copySession(s)
	next.DeleteRequested = true
	next.DeleteError = ""
	if err := b.saveLocked(next, "conversation.deletion_requested", principal); err != nil {
		return err
	}
	m := runnerproto.Frame("session.purge")
	m.SessionID = s.ID
	value := next.Session
	m.Session = &value
	if !b.sendLocked(r, m) {
		return sessionError(409, "runner disconnected before deletion could start")
	}
	return nil
}

func (b *sessionBroker) erasedByRunner(r *connectedRunner, m runnerproto.Message) error {
	b.mu.Lock()
	defer b.mu.Unlock()
	if b.runners[r.id] != r {
		return errors.New("runner connection replaced")
	}
	s := b.sessions[m.SessionID]
	if s == nil {
		ack := runnerproto.Frame("purge.ack")
		ack.SessionID = m.SessionID
		b.sendLocked(r, ack)
		return nil
	}
	if s.PrincipalID != r.principal || s.RunnerID != r.id || s.State != "closed" || !s.DeleteRequested {
		return errors.New("unsolicited conversation purge acknowledgement")
	}
	c, err := b.conversation(r.principal, s.ConversationID)
	if err != nil {
		return err
	}
	if m.Status == "failed" {
		next := copySession(s)
		next.DeleteRequested = false
		next.DeleteError = "O runner não conseguiu apagar os arquivos da conversa. Verifique a máquina e tente novamente."
		if err := b.saveLocked(next, "conversation.deletion_failed", r.principal); err != nil {
			return err
		}
	} else if m.Status == "deleted" {
		if err := b.eraseConversationLocked(c, s); err != nil {
			return err
		}
	} else {
		return errors.New("invalid conversation purge result")
	}
	ack := runnerproto.Frame("purge.ack")
	ack.SessionID = m.SessionID
	b.sendLocked(r, ack)
	return nil
}

func (b *sessionBroker) eraseConversationLocked(c *conversationRecord, s *sessionRecord) error {
	if err := b.store.db.Update(func(tx *bbolt.Tx) error {
		var current conversationRecord
		if err := json.Unmarshal(tx.Bucket([]byte(bucketConversations)).Get([]byte(c.ID)), &current); err != nil {
			return err
		}
		if current.PrincipalID != c.PrincipalID || current.SessionID != s.ID {
			return errors.New("conversation deletion ownership changed")
		}
		owner := tx.Bucket([]byte(bucketConversationIndex)).Bucket([]byte(c.PrincipalID))
		if owner == nil {
			return errors.New("conversation owner index missing")
		}
		index := owner.Bucket([]byte(current.Storage))
		if index == nil {
			return errors.New("conversation category index missing")
		}
		if err := index.Delete(conversationIndexKey(current.OrderAt, c.ID)); err != nil {
			return err
		}
		if err := tx.Bucket([]byte(bucketConversationDue)).Delete(conversationIndexKey(current.ArchiveAfter, c.ID)); err != nil {
			return err
		}
		if current.ArchiveID != "" {
			if err := tx.Bucket([]byte(bucketConversationArchives)).DeleteBucket([]byte(current.ArchiveID)); err != nil {
				return err
			}
		}
		journals := tx.Bucket([]byte(bucketSessionEvents))
		if journals.Bucket([]byte(s.ID)) != nil {
			if err := journals.DeleteBucket([]byte(s.ID)); err != nil {
				return err
			}
		}
		approvals := tx.Bucket([]byte(bucketSessionApprovals))
		var keys [][]byte
		if err := approvals.ForEach(func(key, raw []byte) error {
			var approval approvalRecord
			if err := json.Unmarshal(raw, &approval); err != nil {
				return err
			}
			if approval.SessionID == s.ID && approval.PrincipalID == c.PrincipalID {
				keys = append(keys, bytes.Clone(key))
			}
			return nil
		}); err != nil {
			return err
		}
		for _, key := range keys {
			if err := approvals.Delete(key); err != nil {
				return err
			}
		}
		if err := tx.Bucket([]byte(bucketRunnerSessions)).Delete([]byte(s.ID)); err != nil {
			return err
		}
		if err := tx.Bucket([]byte(bucketConversations)).Delete([]byte(c.ID)); err != nil {
			return err
		}
		return b.store.audit(tx, c.PrincipalID, "conversation.deleted", c.ID, "")
	}); err != nil {
		return err
	}
	delete(b.sessions, s.ID)
	if stream := b.streams[s.ID]; stream != nil {
		for sub := range stream.subscribers {
			close(sub.failed)
		}
		delete(b.streams, s.ID)
	}
	return nil
}
