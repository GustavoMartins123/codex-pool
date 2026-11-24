// Package ctxstore persists native context sessions and envelopes.
package ctxstore

import (
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"unicode/utf8"

	"go.etcd.io/bbolt"
)

const (
	bucketContextSessions   = "native_context_sessions"
	bucketContextSettings   = "native_context_settings"
	bucketContextPrincipals = "native_context_principals"
	contextKeyName          = "envelope_key"
	contextKeySize          = 32
	ParticipantLimit        = 32
	EnvelopeLimit           = 64 << 20
	EnvelopePrefix          = "codex-pool-context-v1:"
)

type Account struct {
	Alias    string `json:"alias"`
	Identity string `json:"identity"`
}

type Session struct {
	Owner        Account   `json:"owner"`
	Participants []Account `json:"participants"`
}

type Result struct {
	Account Account         `json:"account"`
	Value   json.RawMessage `json:"value"`
}

type savedContextSession struct {
	Session
	// Remember renamed aliases even when their physical identity was deduplicated.
	Aliases map[string]string `json:"aliases"`
}

type Store struct {
	db   *bbolt.DB
	aead cipher.AEAD
}

func New(db *bbolt.DB) (*Store, error) {
	if db == nil {
		return nil, errors.New("native context store requires a database")
	}

	var key []byte
	// One transaction makes concurrent constructors share the same persistent key.
	err := db.Update(func(tx *bbolt.Tx) error {
		if _, err := tx.CreateBucketIfNotExists([]byte(bucketContextSessions)); err != nil {
			return err
		}
		if _, err := tx.CreateBucketIfNotExists([]byte(bucketContextPrincipals)); err != nil {
			return err
		}
		settings, err := tx.CreateBucketIfNotExists([]byte(bucketContextSettings))
		if err != nil {
			return err
		}
		stored := settings.Get([]byte(contextKeyName))
		if stored != nil {
			if len(stored) != contextKeySize {
				return errors.New("invalid native context envelope key")
			}
			key = append([]byte(nil), stored...)
			return nil
		}

		key = make([]byte, contextKeySize)
		if _, err := rand.Read(key); err != nil {
			return err
		}
		return settings.Put([]byte(contextKeyName), key)
	})
	if err != nil {
		return nil, fmt.Errorf("initialize native context store: %w", err)
	}

	block, err := aes.NewCipher(key)
	if err != nil {
		return nil, err
	}
	aead, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	return &Store{db: db, aead: aead}, nil
}

func contextTuple(scope, session string) ([]byte, error) {
	// JSON replaces invalid UTF-8, which could otherwise collapse distinct keys.
	if !utf8.ValidString(scope) || !utf8.ValidString(session) {
		return nil, errors.New("native context scope and session must be valid UTF-8")
	}
	return json.Marshal([2]string{scope, session})
}

func (s *Store) Claim(scope, session string) error {
	if scope == "" || session == "" || !utf8.ValidString(scope) || !utf8.ValidString(session) {
		return errors.New("invalid context principal or session")
	}
	// The backend sees only session_id, so local tuple keys alone cannot isolate pool users.
	return s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket([]byte(bucketContextPrincipals))
		owner := bucket.Get([]byte(session))
		if owner != nil {
			if string(owner) != scope {
				return errors.New("context session belongs to another principal")
			}
			return nil
		}
		return bucket.Put([]byte(session), []byte(scope))
	})
}

func (s *Store) Lookup(scope, session string) (*Session, error) {
	key, err := contextTuple(scope, session)
	if err != nil {
		return nil, err
	}
	var result *Session
	err = s.db.View(func(tx *bbolt.Tx) error {
		data := tx.Bucket([]byte(bucketContextSessions)).Get(key)
		if data == nil {
			return nil
		}
		result = new(Session)
		return json.Unmarshal(data, result)
	})
	if err != nil {
		return nil, err
	}
	return result, nil
}

func (s *Store) Record(scope, session string, account Account) (*Session, error) {
	key, err := contextTuple(scope, session)
	if err != nil {
		return nil, err
	}
	var saved savedContextSession
	err = s.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket([]byte(bucketContextSessions))
		data := bucket.Get(key)
		if data == nil {
			saved.Owner = account
			saved.Aliases = make(map[string]string)
		} else if err := json.Unmarshal(data, &saved); err != nil {
			return err
		}

		if identity, found := saved.Aliases[account.Alias]; found && identity != account.Identity {
			return errors.New("native context account alias changed identity")
		}
		known := false
		for _, participant := range saved.Participants {
			if participant.Identity == account.Identity {
				known = true
				break
			}
		}
		if !known {
			if len(saved.Participants) >= ParticipantLimit {
				return errors.New("native context participant limit exceeded")
			}
			saved.Participants = append(saved.Participants, account)
		}
		if saved.Aliases == nil {
			return errors.New("native context session has no alias bindings")
		}
		saved.Aliases[account.Alias] = account.Identity

		encoded, err := json.Marshal(saved)
		if err != nil {
			return err
		}
		return bucket.Put(key, encoded)
	})
	if err != nil {
		return nil, err
	}
	return &saved.Session, nil
}

func (s *Store) Pack(scope, session string, sources []Result) (string, error) {
	tuple, err := contextTuple(scope, session)
	if err != nil {
		return "", err
	}
	remaining := EnvelopeLimit
	for _, source := range sources {
		if len(source.Value) > remaining {
			return "", errors.New("native context envelope limit exceeded")
		}
		remaining -= len(source.Value)
	}
	plaintext, err := json.Marshal(sources)
	if err != nil {
		return "", err
	}
	if len(plaintext) > EnvelopeLimit {
		return "", errors.New("native context envelope limit exceeded")
	}

	nonce := make([]byte, s.aead.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return "", err
	}
	aad := append([]byte(EnvelopePrefix), tuple...)
	payload := s.aead.Seal(nonce, nonce, plaintext, aad)
	return EnvelopePrefix + base64.RawURLEncoding.EncodeToString(payload), nil
}

func (s *Store) Unpack(scope, session, encoded string) ([]Result, error) {
	tuple, err := contextTuple(scope, session)
	if err != nil {
		return nil, err
	}
	if !strings.HasPrefix(encoded, EnvelopePrefix) {
		return nil, errors.New("invalid native context envelope version")
	}
	body := strings.TrimPrefix(encoded, EnvelopePrefix)
	maxPayload := EnvelopeLimit + s.aead.NonceSize() + s.aead.Overhead()
	// Bound the encoded input before base64 allocates a decoded buffer.
	if len(body) > base64.RawURLEncoding.EncodedLen(maxPayload) {
		return nil, errors.New("native context envelope limit exceeded")
	}
	if strings.ContainsAny(body, "\r\n") {
		return nil, errors.New("invalid native context envelope encoding")
	}
	payload, err := base64.RawURLEncoding.Strict().DecodeString(body)
	if err != nil {
		return nil, fmt.Errorf("decode native context envelope: %w", err)
	}
	if len(payload) < s.aead.NonceSize()+s.aead.Overhead() {
		return nil, errors.New("native context envelope is truncated")
	}
	aad := append([]byte(EnvelopePrefix), tuple...)
	nonce := payload[:s.aead.NonceSize()]
	plaintext, err := s.aead.Open(nil, nonce, payload[s.aead.NonceSize():], aad)
	if err != nil {
		return nil, fmt.Errorf("authenticate native context envelope: %w", err)
	}
	var sources []Result
	if err := json.Unmarshal(plaintext, &sources); err != nil {
		return nil, fmt.Errorf("decode native context results: %w", err)
	}
	return sources, nil
}
