package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"

	"go.etcd.io/bbolt"
)

const bucketSessionEvents = "session_events"

func newSessionEvent(kind string, value any) sessionEvent {
	data, err := json.Marshal(value)
	if err != nil {
		panic(err)
	}
	return sessionEvent{Type: kind, Data: data}
}

func eventKey(id uint64) []byte {
	key := make([]byte, 8)
	binary.BigEndian.PutUint64(key, id)
	return key
}

// The session, runner acknowledgement sequence and journal advance in one transaction.
func (b *sessionBroker) recordEventsTx(tx *bbolt.Tx, s *sessionRecord, events []sessionEvent) error {
	if len(events) > 0 {
		journal, err := tx.Bucket([]byte(bucketSessionEvents)).CreateBucketIfNotExists([]byte(s.ID))
		if err != nil {
			return err
		}
		for i := range events {
			s.EventCursor++
			events[i].ID = s.EventCursor
			events[i].At = b.now()
			if err := putJSON(journal, string(eventKey(s.EventCursor)), events[i]); err != nil {
				return err
			}
		}
	}
	if err := b.updateConversationTx(tx, s, events); err != nil {
		return err
	}
	return putJSON(tx.Bucket([]byte(bucketRunnerSessions)), s.ID, s)
}

// A missing or corrupt range is an error; never return an apparently complete partial chat.
func (b *sessionBroker) readEventsLocked(s *sessionRecord, after, through uint64, limit int) ([]sessionEvent, error) {
	if after > through || through > s.EventCursor {
		return nil, sessionError(409, "invalid history cursor")
	}
	events := []sessionEvent{}
	err := b.store.db.View(func(tx *bbolt.Tx) error {
		if after == through {
			return nil
		}
		var c conversationRecord
		if err := json.Unmarshal(tx.Bucket([]byte(bucketConversations)).Get([]byte(s.ConversationID)), &c); err != nil {
			return err
		}
		if c.PrincipalID != s.PrincipalID || c.SessionID != s.ID {
			return errors.New("conversation ownership mismatch")
		}
		reader, err := newConversationEventReader(tx, &c)
		if err != nil {
			return err
		}
		size := 0
		for expected := after + 1; expected <= through; expected++ {
			event, err := reader.event(expected)
			if err != nil {
				return err
			}
			events = append(events, event)
			size += len(event.Data)
			if limit > 0 && (len(events) >= limit || size >= 512<<10) {
				break
			}
		}
		return nil
	})
	return events, err
}
