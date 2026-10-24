package main

import (
	"bytes"
	"codex-pool-proxy/internal/runnerproto"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"

	"github.com/google/uuid"
	"github.com/klauspost/compress/zstd"
	"go.etcd.io/bbolt"
)

const bucketConversationArchives = "conversation_archives"
const conversationBlockBytes = 256 << 10

type conversationArchiveBlock struct {
	First        uint64 `json:"first"`
	Last         uint64 `json:"last"`
	RawBytes     int    `json:"raw_bytes"`
	EncodedBytes int    `json:"encoded_bytes"`
	RawSHA256    string `json:"raw_sha256"`
	SHA256       string `json:"sha256"`
}

type conversationArchive struct {
	Version        int                        `json:"version"`
	ID             string                     `json:"id"`
	PrincipalID    string                     `json:"principal_id"`
	ConversationID string                     `json:"conversation_id"`
	Through        uint64                     `json:"through"`
	Codec          string                     `json:"codec"`
	Blocks         []conversationArchiveBlock `json:"blocks"`
}

func archiveDigest(raw []byte) string {
	value := sha256.Sum256(raw)
	return hex.EncodeToString(value[:])
}

func archiveManifestTx(tx *bbolt.Tx, c *conversationRecord) (*conversationArchive, error) {
	if c.ArchiveID == "" {
		if c.ArchivedThrough != 0 {
			return nil, errors.New("archive prefix omitted manifest")
		}
		return nil, nil
	}
	bucket := tx.Bucket([]byte(bucketConversationArchives)).Bucket([]byte(c.ArchiveID))
	if bucket == nil {
		return nil, errors.New("conversation archive missing")
	}
	var manifest conversationArchive
	if err := json.Unmarshal(bucket.Get([]byte("manifest")), &manifest); err != nil {
		return nil, err
	}
	if manifest.Version != 1 || manifest.ID != c.ArchiveID || manifest.PrincipalID != c.PrincipalID || manifest.ConversationID != c.ID || manifest.Through != c.ArchivedThrough || manifest.Codec != "zstd-fastest-256k-v1" {
		return nil, errors.New("conversation archive identity or format mismatch")
	}
	var expected uint64 = 1
	for _, block := range manifest.Blocks {
		if block.First != expected || block.Last < block.First || block.RawBytes <= 0 || block.RawBytes > 512<<10 || block.EncodedBytes <= 0 || block.EncodedBytes > 1<<20 {
			return nil, errors.New("conversation archive manifest range invalid")
		}
		expected = block.Last + 1
	}
	if expected != manifest.Through+1 {
		return nil, errors.New("conversation archive is incomplete")
	}
	return &manifest, nil
}

func decodeArchiveBlock(tx *bbolt.Tx, manifest *conversationArchive, index int) ([][]byte, error) {
	block := manifest.Blocks[index]
	encoded := tx.Bucket([]byte(bucketConversationArchives)).Bucket([]byte(manifest.ID)).Get(eventKey(block.First))
	if len(encoded) != block.EncodedBytes || archiveDigest(encoded) != block.SHA256 {
		return nil, errors.New("conversation archive block checksum mismatch")
	}
	decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(2<<20))
	if err != nil {
		return nil, err
	}
	defer decoder.Close()
	raw, err := decoder.DecodeAll(encoded, make([]byte, 0, block.RawBytes))
	if err != nil {
		return nil, err
	}
	if len(raw) != block.RawBytes || archiveDigest(raw) != block.RawSHA256 {
		return nil, errors.New("conversation archive content checksum mismatch")
	}
	lines := bytes.Split(raw, []byte{'\n'})
	if len(lines) != int(block.Last-block.First+1)+1 || len(lines[len(lines)-1]) != 0 {
		return nil, errors.New("conversation archive event framing invalid")
	}
	lines = lines[:len(lines)-1]
	for i, line := range lines {
		var e sessionEvent
		if err := json.Unmarshal(line, &e); err != nil {
			return nil, err
		}
		if e.ID != block.First+uint64(i) || e.Type == "" || !json.Valid(e.Data) {
			return nil, errors.New("conversation archive event invalid")
		}
	}
	return lines, nil
}

type conversationEventReader struct {
	tx         *bbolt.Tx
	c          *conversationRecord
	manifest   *conversationArchive
	blockIndex int
	block      [][]byte
}

func newConversationEventReader(tx *bbolt.Tx, c *conversationRecord) (*conversationEventReader, error) {
	manifest, err := archiveManifestTx(tx, c)
	if err != nil {
		return nil, err
	}
	return &conversationEventReader{tx: tx, c: c, manifest: manifest, blockIndex: -1}, nil
}

func (r *conversationEventReader) raw(id uint64) ([]byte, error) {
	if id == 0 || id > r.c.EventCursor {
		return nil, errors.New("event outside conversation range")
	}
	if id <= r.c.ArchivedThrough {
		if r.manifest == nil {
			return nil, errors.New("conversation archive manifest unavailable")
		}
		index := sort.Search(len(r.manifest.Blocks), func(i int) bool { return r.manifest.Blocks[i].Last >= id })
		if index == len(r.manifest.Blocks) {
			return nil, errors.New("conversation archive range missing")
		}
		if index != r.blockIndex {
			block, err := decodeArchiveBlock(r.tx, r.manifest, index)
			if err != nil {
				return nil, err
			}
			r.block = block
			r.blockIndex = index
		}
		return r.block[id-r.manifest.Blocks[index].First], nil
	}
	journal := r.tx.Bucket([]byte(bucketSessionEvents)).Bucket([]byte(r.c.SessionID))
	if journal == nil {
		return nil, errors.New("conversation journal missing")
	}
	raw := journal.Get(eventKey(id))
	if raw == nil {
		return nil, fmt.Errorf("conversation event %d missing", id)
	}
	return raw, nil
}

func (r *conversationEventReader) event(id uint64) (sessionEvent, error) {
	raw, err := r.raw(id)
	if err != nil {
		return sessionEvent{}, err
	}
	var event sessionEvent
	if err := json.Unmarshal(raw, &event); err != nil {
		return event, err
	}
	if event.ID != id || event.Type == "" || !json.Valid(event.Data) {
		return event, errors.New("conversation event corrupt")
	}
	return event, nil
}

func (b *sessionBroker) archiveConversation(principal, id string) error {
	c, err := b.conversation(principal, id)
	if err != nil {
		return err
	}
	if c.Storage == "archived" {
		return nil
	}
	if !c.HistoryComplete || c.Deleting || c.ArchiveAfter.After(b.now()) || c.SessionState != "closed" {
		return sessionError(409, "conversation is not eligible for archival")
	}
	buildID := uuid.NewString()
	manifest := conversationArchive{Version: 1, ID: buildID, PrincipalID: principal, ConversationID: id, Through: c.EventCursor, Codec: "zstd-fastest-256k-v1", Blocks: []conversationArchiveBlock{}}
	encoder, err := zstd.NewWriter(nil, zstd.WithEncoderLevel(zstd.SpeedFastest), zstd.WithEncoderConcurrency(1), zstd.WithWindowSize(1<<20), zstd.WithEncoderCRC(true))
	if err != nil {
		return err
	}
	defer encoder.Close()
	if err := b.store.db.Update(func(tx *bbolt.Tx) error {
		_, err := tx.Bucket([]byte(bucketConversationArchives)).CreateBucket([]byte(buildID))
		return err
	}); err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			_ = b.store.db.Update(func(tx *bbolt.Tx) error {
				return tx.Bucket([]byte(bucketConversationArchives)).DeleteBucket([]byte(buildID))
			})
		}
	}()
	var after uint64
	for after < c.EventCursor {
		var raw bytes.Buffer
		first := after + 1
		err := b.store.db.View(func(tx *bbolt.Tx) error {
			var current conversationRecord
			if err := json.Unmarshal(tx.Bucket([]byte(bucketConversations)).Get([]byte(id)), &current); err != nil {
				return err
			}
			if current.Generation != c.Generation || current.EventCursor != c.EventCursor {
				return sessionError(409, "conversation changed during archival")
			}
			reader, err := newConversationEventReader(tx, c)
			if err != nil {
				return err
			}
			for next := after + 1; next <= c.EventCursor; next++ {
				value, err := reader.raw(next)
				if err != nil {
					return err
				}
				var event sessionEvent
				if err := json.Unmarshal(value, &event); err != nil {
					return err
				}
				if event.ID != next || !json.Valid(event.Data) {
					return errors.New("cannot archive corrupt event")
				}
				if raw.Len() > 0 && raw.Len()+len(value)+1 > conversationBlockBytes {
					break
				}
				if len(value)+1 > 512<<10 {
					return errors.New("conversation event exceeds archive limit")
				}
				raw.Write(value)
				raw.WriteByte('\n')
				after = next
			}
			return nil
		})
		if err != nil {
			return err
		}
		encoded := encoder.EncodeAll(raw.Bytes(), nil)
		// Verify lossless compression before making the block eligible for publication.
		decoder, err := zstd.NewReader(nil, zstd.WithDecoderConcurrency(1), zstd.WithDecoderMaxMemory(2<<20))
		if err != nil {
			return err
		}
		decoded, err := decoder.DecodeAll(encoded, nil)
		decoder.Close()
		if err != nil {
			return err
		}
		if !bytes.Equal(decoded, raw.Bytes()) {
			return errors.New("archive roundtrip verification failed")
		}
		block := conversationArchiveBlock{First: first, Last: after, RawBytes: raw.Len(), EncodedBytes: len(encoded), RawSHA256: archiveDigest(raw.Bytes()), SHA256: archiveDigest(encoded)}
		if err := b.store.db.Update(func(tx *bbolt.Tx) error {
			return tx.Bucket([]byte(bucketConversationArchives)).Bucket([]byte(buildID)).Put(eventKey(first), encoded)
		}); err != nil {
			return err
		}
		manifest.Blocks = append(manifest.Blocks, block)
	}
	b.mu.Lock()
	defer b.mu.Unlock()
	err = b.store.db.Update(func(tx *bbolt.Tx) error {
		var current conversationRecord
		if err := json.Unmarshal(tx.Bucket([]byte(bucketConversations)).Get([]byte(id)), &current); err != nil {
			return err
		}
		s := b.sessions[current.SessionID]
		if s == nil || s.State != "closed" || len(s.Approvals) > 0 || current.Generation != c.Generation || current.EventCursor != c.EventCursor || current.Storage != "active" || current.ArchiveAfter.After(b.now()) {
			return sessionError(409, "conversation changed before archive commit")
		}
		if err := putJSON(tx.Bucket([]byte(bucketConversationArchives)).Bucket([]byte(buildID)), "manifest", manifest); err != nil {
			return err
		}
		next := current
		next.Storage = "archived"
		next.ArchiveID = buildID
		next.ArchivedThrough = c.EventCursor
		next.Generation++
		// Index, canonical prefix and removal of hot copies commit atomically.
		if err := putConversationTx(tx, &current, &next); err != nil {
			return err
		}
		journals := tx.Bucket([]byte(bucketSessionEvents))
		if journals.Bucket([]byte(s.ID)) != nil {
			if err := journals.DeleteBucket([]byte(s.ID)); err != nil {
				return err
			}
		}
		if current.ArchiveID != "" {
			if err := tx.Bucket([]byte(bucketConversationArchives)).DeleteBucket([]byte(current.ArchiveID)); err != nil {
				return err
			}
		}
		return b.store.audit(tx, principal, "conversation.archived", id, "")
	})
	if err == nil {
		published = true
	}
	return err
}

func (b *sessionBroker) archiveDueConversations() error {
	now := b.now()
	var candidates []conversationRecord
	if err := b.store.db.View(func(tx *bbolt.Tx) error {
		cursor := tx.Bucket([]byte(bucketConversationDue)).Cursor()
		for key, id := cursor.First(); key != nil && len(candidates) < 32; key, id = cursor.Next() {
			if len(key) <= 8 {
				return errors.New("invalid archive due index")
			}
			if binary.BigEndian.Uint64(key[:8]) > uint64(now.UnixNano()) {
				break
			}
			var c conversationRecord
			if err := json.Unmarshal(tx.Bucket([]byte(bucketConversations)).Get(id), &c); err != nil {
				return err
			}
			if !slices.Equal(key, conversationIndexKey(c.ArchiveAfter, c.ID)) {
				return errors.New("stale archive due index")
			}
			candidates = append(candidates, c)
		}
		return nil
	}); err != nil {
		return err
	}
	for _, c := range candidates {
		if c.SessionState == "waiting_input" {
			b.mu.Lock()
			s := b.sessions[c.SessionID]
			if s != nil && s.State == "waiting_input" && s.TurnID == "" && len(s.Approvals) == 0 {
				r := b.runners[s.RunnerID]
				if r != nil && r.hello.PersistentSessions && s.LeaseExpiresAt.After(now) {
					next := copySession(s)
					next.State = "closing"
					next.ClosedReason = "archived"
					next.UpdatedAt = now
					if err := b.saveLocked(next, "session.suspending", c.PrincipalID, newSessionEvent("state", map[string]any{"state": "closing"})); err != nil {
						b.mu.Unlock()
						return err
					}
					m := runnerproto.Frame("session.close")
					m.SessionID = s.ID
					b.sendLocked(r, m)
				}
			}
			b.mu.Unlock()
			continue
		}
		if err := b.archiveConversation(c.PrincipalID, c.ID); err != nil {
			var fault *sessionFault
			if errors.As(err, &fault) && fault.status == 409 {
				continue
			}
			return err
		}
	}
	return nil
}

func cleanupArchiveBuildsTx(tx *bbolt.Tx) error {
	referenced := map[string]bool{}
	if err := tx.Bucket([]byte(bucketConversations)).ForEach(func(_, raw []byte) error {
		var c conversationRecord
		if err := json.Unmarshal(raw, &c); err != nil {
			return err
		}
		if c.ArchiveID != "" {
			referenced[c.ArchiveID] = true
		}
		return nil
	}); err != nil {
		return err
	}
	root := tx.Bucket([]byte(bucketConversationArchives))
	var stale [][]byte
	if err := root.ForEach(func(key, _ []byte) error {
		if !referenced[string(key)] {
			stale = append(stale, bytes.Clone(key))
		}
		return nil
	}); err != nil {
		return err
	}
	for _, key := range stale {
		if err := root.DeleteBucket(key); err != nil {
			return err
		}
	}
	return nil
}
