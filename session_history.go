package main

import (
	"encoding/binary"
	"encoding/json"
	"errors"
	"fmt"

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
		journal := tx.Bucket([]byte(bucketSessionEvents)).Bucket([]byte(s.ID))
		if journal == nil {
			return errors.New("session event journal missing")
		}
		cursor := journal.Cursor()
		expected := after + 1
		bytes := 0
		for key, raw := cursor.Seek(eventKey(expected)); expected <= through; key, raw = cursor.Next() {
			var event sessionEvent
			if len(key) != 8 || binary.BigEndian.Uint64(key) != expected {
				return fmt.Errorf("session event journal gap at %d", expected)
			}
			if err := json.Unmarshal(raw, &event); err != nil {
				return err
			}
			if event.ID != expected || event.Type == "" || !json.Valid(event.Data) {
				return errors.New("invalid session event journal entry")
			}
			events = append(events, event)
			expected++
			bytes += len(raw)
			if limit > 0 && (len(events) >= limit || bytes >= 512<<10) {
				break
			}
		}
		return nil
	})
	return events, err
}
