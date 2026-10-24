package main

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"go.etcd.io/bbolt"
)

const (
	bucketConversations      = "conversations"
	bucketConversationIndex  = "conversations_by_principal"
	bucketConversationDue    = "conversation_archive_due"
	bucketConversationSchema = "conversation_schema"
	conversationIdleTTL      = 7 * 24 * time.Hour
)

type conversationPreview struct {
	TurnID string `json:"turn_id"`
	Role   string `json:"role"`
	Text   string `json:"text"`
}

type conversationRecord struct {
	ID                string                `json:"id"`
	PrincipalID       string                `json:"principal_id"`
	SessionID         string                `json:"session_id"`
	Agent             string                `json:"agent"`
	Model             string                `json:"model"`
	Title             string                `json:"title"`
	Storage           string                `json:"storage"`
	SessionState      string                `json:"session_state"`
	CreatedAt         time.Time             `json:"created_at"`
	LastInteractionAt *time.Time            `json:"last_interaction_at"`
	OrderAt           time.Time             `json:"order_at"`
	ArchiveAfter      time.Time             `json:"archive_after"`
	MigratedAt        *time.Time            `json:"migrated_at,omitempty"`
	EventCursor       uint64                `json:"event_cursor"`
	Generation        uint64                `json:"generation"`
	HistoryComplete   bool                  `json:"history_complete"`
	Preview           []conversationPreview `json:"preview"`
	ResumeAvailable   bool                  `json:"resume_available"`
	ArchiveID         string                `json:"archive_id,omitempty"`
	ArchivedThrough   uint64                `json:"archived_through"`
}

func conversationIndexKey(at time.Time, id string) []byte {
	return append(eventKey(uint64(at.UnixNano())), []byte(id)...)
}

func boundedPreview(value string, size int) string {
	runes := []rune(value)
	return string(runes[:min(size, len(runes))])
}

func (c *conversationRecord) applyEvent(event sessionEvent) error {
	var data struct {
		TurnID  string `json:"turn_id"`
		Input   string `json:"input"`
		Kind    string `json:"kind"`
		Content string `json:"content"`
		Source  string `json:"source"`
	}
	if err := json.Unmarshal(event.Data, &data); err != nil {
		return err
	}
	interacted := event.Type == "turn.input" || event.Type == "turn.finished" ||
		(event.Type == "turn.delta" && data.Content != "") || (event.Type == "approval.resolved" && data.Source == "user")
	if event.Type == "state" && data.Source == "user_resume" {
		interacted = true
		c.Storage = "active"
	}
	if interacted && !event.At.IsZero() {
		at := event.At
		c.LastInteractionAt = &at
		c.OrderAt = at
		c.ArchiveAfter = at.Add(conversationIdleTTL)
	}
	role, text := "", ""
	if event.Type == "turn.input" {
		role, text = "user", data.Input
	}
	if event.Type == "turn.delta" && data.Kind == "text" {
		role, text = "assistant", data.Content
	}
	if role == "" {
		return nil
	}
	if data.TurnID == "" {
		return errors.New("conversation message omitted turn ID")
	}
	if c.Title == "" && role == "user" {
		c.Title = boundedPreview(strings.TrimSpace(text), 80)
	}
	last := len(c.Preview) - 1
	if last >= 0 && c.Preview[last].TurnID == data.TurnID && c.Preview[last].Role == role {
		if len([]rune(c.Preview[last].Text)) < 512 {
			c.Preview[last].Text = boundedPreview(c.Preview[last].Text+text, 512)
		}
	} else {
		c.Preview = append(c.Preview, conversationPreview{data.TurnID, role, boundedPreview(text, 512)})
		if len(c.Preview) > 3 {
			c.Preview = c.Preview[len(c.Preview)-3:]
		}
	}
	return nil
}

func putConversationTx(tx *bbolt.Tx, previous *conversationRecord, next *conversationRecord) error {
	if next.ID == "" || next.PrincipalID == "" || next.SessionID == "" || next.OrderAt.IsZero() ||
		(next.Storage != "active" && next.Storage != "archived") {
		return errors.New("invalid conversation metadata")
	}
	index, err := tx.Bucket([]byte(bucketConversationIndex)).CreateBucketIfNotExists([]byte(next.PrincipalID))
	if err != nil {
		return err
	}
	if previous != nil {
		if previous.PrincipalID != next.PrincipalID || previous.ID != next.ID {
			return errors.New("conversation owner is immutable")
		}
		bucket := index.Bucket([]byte(previous.Storage))
		if bucket == nil {
			return errors.New("conversation index missing")
		}
		if err := bucket.Delete(conversationIndexKey(previous.OrderAt, previous.ID)); err != nil {
			return err
		}
		if previous.Storage == "active" {
			if err := tx.Bucket([]byte(bucketConversationDue)).Delete(conversationIndexKey(previous.ArchiveAfter, previous.ID)); err != nil {
				return err
			}
		}
	}
	bucket, err := index.CreateBucketIfNotExists([]byte(next.Storage))
	if err != nil {
		return err
	}
	if err := bucket.Put(conversationIndexKey(next.OrderAt, next.ID), []byte(next.ID)); err != nil {
		return err
	}
	if next.Storage == "active" && next.HistoryComplete && (next.SessionState == "closed" || next.SessionState == "waiting_input") {
		if err := tx.Bucket([]byte(bucketConversationDue)).Put(conversationIndexKey(next.ArchiveAfter, next.ID), []byte(next.ID)); err != nil {
			return err
		}
	}
	return putJSON(tx.Bucket([]byte(bucketConversations)), next.ID, next)
}

func (b *sessionBroker) updateConversationTx(tx *bbolt.Tx, s *sessionRecord, events []sessionEvent) error {
	var c conversationRecord
	raw := tx.Bucket([]byte(bucketConversations)).Get([]byte(s.ConversationID))
	var previous *conversationRecord
	if raw == nil {
		if s.ConversationID != s.ID || tx.Bucket([]byte(bucketRunnerSessions)).Get([]byte(s.ID)) != nil {
			return errors.New("conversation record missing")
		}
		c = conversationRecord{ID: s.ConversationID, PrincipalID: s.PrincipalID, SessionID: s.ID, Agent: s.Agent, Model: s.Model, Storage: "active", CreatedAt: s.CreatedAt, OrderAt: s.CreatedAt,
			ArchiveAfter: s.CreatedAt.Add(conversationIdleTTL), Generation: 1, Preview: []conversationPreview{}}
	} else {
		if err := json.Unmarshal(raw, &c); err != nil {
			return err
		}
		copy := c
		previous = &copy
		if c.PrincipalID != s.PrincipalID || c.SessionID != s.ID {
			return errors.New("conversation session ownership mismatch")
		}
	}
	for _, event := range events {
		if err := c.applyEvent(event); err != nil {
			return err
		}
	}
	if len(events) > 0 {
		c.Generation++
	}
	c.SessionState = s.State
	c.ResumeAvailable = s.State == "closed" && s.TurnID == "" && len(s.Approvals) == 0 && s.Checkpoint != nil && s.Checkpoint.Ready
	c.EventCursor = s.EventCursor
	c.HistoryComplete = s.HistoryComplete
	return putConversationTx(tx, previous, &c)
}

// Explicit, transactional conversion: old journal bytes and event IDs are retained.
// Old events lack interaction timestamps, so those remain unknown. A separate
// migration grace period prevents premature archival without inventing activity.
func (b *sessionBroker) migrateConversationsTx(tx *bbolt.Tx) error {
	schema := tx.Bucket([]byte(bucketConversationSchema))
	if raw := schema.Get([]byte("version")); raw != nil {
		if bytes.Equal(raw, []byte("1")) {
			if err := tx.DeleteBucket([]byte(bucketConversationDue)); err != nil {
				return err
			}
			due, err := tx.CreateBucket([]byte(bucketConversationDue))
			if err != nil {
				return err
			}
			if err := tx.Bucket([]byte(bucketConversations)).ForEach(func(_, raw []byte) error {
				var c conversationRecord
				if err := json.Unmarshal(raw, &c); err != nil {
					return err
				}
				if c.Storage == "active" && c.HistoryComplete && (c.SessionState == "closed" || c.SessionState == "waiting_input") {
					return due.Put(conversationIndexKey(c.ArchiveAfter, c.ID), []byte(c.ID))
				}
				return nil
			}); err != nil {
				return err
			}
			return schema.Put([]byte("version"), []byte("2"))
		}
		if !bytes.Equal(raw, []byte("2")) {
			return errors.New("unsupported conversation schema")
		}
		return nil
	}
	var sessions []sessionRecord
	if err := tx.Bucket([]byte(bucketRunnerSessions)).ForEach(func(_, raw []byte) error {
		var s sessionRecord
		if err := json.Unmarshal(raw, &s); err != nil {
			return err
		}
		sessions = append(sessions, s)
		return nil
	}); err != nil {
		return err
	}
	for _, s := range sessions {
		if s.ConversationID != "" {
			return errors.New("conversation migration encountered unversioned metadata")
		}
		at := b.now()
		c := conversationRecord{ID: s.ID, PrincipalID: s.PrincipalID, SessionID: s.ID, Agent: s.Agent, Model: s.Model, Storage: "active", SessionState: s.State, CreatedAt: s.CreatedAt, OrderAt: at,
			ArchiveAfter: at.Add(conversationIdleTTL), MigratedAt: &at, Generation: 1, EventCursor: s.EventCursor, HistoryComplete: s.HistoryComplete, Preview: []conversationPreview{}}
		journal := tx.Bucket([]byte(bucketSessionEvents)).Bucket([]byte(s.ID))
		if s.HistoryComplete && s.EventCursor > 0 {
			if journal == nil {
				return errors.New("migration requires the complete session journal")
			}
			cursor := journal.Cursor()
			var id uint64
			for key, raw := cursor.First(); key != nil; key, raw = cursor.Next() {
				id++
				var event sessionEvent
				if !bytes.Equal(key, eventKey(id)) {
					return errors.New("migration found a journal gap")
				}
				if err := json.Unmarshal(raw, &event); err != nil {
					return err
				}
				if event.ID != id {
					return errors.New("migration found an invalid event ID")
				}
				if err := c.applyEvent(event); err != nil {
					return err
				}
			}
			if id != s.EventCursor {
				return errors.New("migration found an incomplete session journal")
			}
		}
		s.ConversationID = s.ID
		if err := putConversationTx(tx, nil, &c); err != nil {
			return err
		}
		if err := putJSON(tx.Bucket([]byte(bucketRunnerSessions)), s.ID, s); err != nil {
			return err
		}
	}
	return schema.Put([]byte("version"), []byte("2"))
}

type conversationListCursor struct {
	Version   int    `json:"v"`
	Principal string `json:"principal"`
	Storage   string `json:"storage"`
	Key       []byte `json:"key"`
}

type conversationListPage struct {
	Conversations []conversationRecord `json:"conversations"`
	NextCursor    string               `json:"next_cursor,omitempty"`
}

func (b *sessionBroker) conversation(principal, id string) (*conversationRecord, error) {
	var c conversationRecord
	err := b.store.db.View(func(tx *bbolt.Tx) error {
		raw := tx.Bucket([]byte(bucketConversations)).Get([]byte(id))
		if raw == nil {
			return sessionError(404, "conversation not found")
		}
		if err := json.Unmarshal(raw, &c); err != nil {
			return err
		}
		if c.PrincipalID != principal {
			return sessionError(404, "conversation not found")
		}
		return nil
	})
	return &c, err
}

func (b *sessionBroker) conversations(principal, storage, cursorToken string, limit int) (*conversationListPage, error) {
	if (storage != "active" && storage != "archived") || limit < 1 || limit > 100 {
		return nil, sessionError(400, "invalid conversation list parameters")
	}
	var after []byte
	if cursorToken != "" {
		if len(cursorToken) > 2048 {
			return nil, sessionError(400, "invalid conversation cursor")
		}
		raw, err := base64.RawURLEncoding.DecodeString(cursorToken)
		if err != nil {
			return nil, sessionError(400, "invalid conversation cursor")
		}
		var cursor conversationListCursor
		if json.Unmarshal(raw, &cursor) != nil || cursor.Version != 1 || cursor.Principal != principal || cursor.Storage != storage || len(cursor.Key) <= 8 {
			return nil, sessionError(400, "invalid conversation cursor scope")
		}
		after = cursor.Key
	}
	page := &conversationListPage{Conversations: []conversationRecord{}}
	err := b.store.db.View(func(tx *bbolt.Tx) error {
		owner := tx.Bucket([]byte(bucketConversationIndex)).Bucket([]byte(principal))
		if owner == nil {
			return nil
		}
		index := owner.Bucket([]byte(storage))
		if index == nil {
			return nil
		}
		cursor := index.Cursor()
		key, value := cursor.Last()
		if after != nil {
			key, value = cursor.Seek(after)
			if key == nil {
				key, value = cursor.Last()
			} else {
				key, value = cursor.Prev()
			}
		}
		for ; key != nil; key, value = cursor.Prev() {
			if len(page.Conversations) == limit {
				last := page.Conversations[len(page.Conversations)-1]
				raw, err := json.Marshal(conversationListCursor{1, principal, storage, conversationIndexKey(last.OrderAt, last.ID)})
				if err != nil {
					return err
				}
				page.NextCursor = base64.RawURLEncoding.EncodeToString(raw)
				break
			}
			var c conversationRecord
			raw := tx.Bucket([]byte(bucketConversations)).Get(value)
			if raw == nil {
				return errors.New("conversation index points to missing record")
			}
			if err := json.Unmarshal(raw, &c); err != nil {
				return err
			}
			if c.PrincipalID != principal || c.Storage != storage || !bytes.Equal(key, conversationIndexKey(c.OrderAt, c.ID)) {
				return fmt.Errorf("invalid conversation index entry")
			}
			page.Conversations = append(page.Conversations, c)
		}
		return nil
	})
	return page, err
}
