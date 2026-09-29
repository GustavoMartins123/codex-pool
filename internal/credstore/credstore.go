// Package credstore provides at-rest encryption for upstream credential
// files (pool/<provider>/<account>.json). The envelope format is versioned
// and key-versioned so master keys can rotate without a big-bang rewrite.
//
// Layout on disk (JSON envelope):
//
//	{"cpvault":1,"kv":2,"nonce":"<base64>","ct":"<base64>"}
//
// "kv" is the key version that produced the ciphertext. Readers accept the
// current key plus one previous key (rotation window); anything else fails
// closed. The AAD binds the format identity, not the file path, so encrypted
// files stay portable across pool directories and backup restores.
package credstore

import (
	"strings"
	"crypto/aes"
	"crypto/cipher"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
)

const aadPrefix = "codex-pool/credstore/v1"

var (
	// ErrNotEncrypted indicates the payload is not a vault envelope.
	ErrNotEncrypted = errors.New("credstore: payload is not an encrypted envelope")
	// ErrUnknownKeyVersion indicates no configured key can decrypt the envelope.
	ErrUnknownKeyVersion = errors.New("credstore: envelope key version is not configured")
)

// Key is a versioned AES-256 master key.
type Key struct {
	Version int
	aesKey  []byte
}

// ParseKey materializes a Key. Accepted forms:
//   - 64 hex chars: used verbatim as the AES-256 key.
//   - any string with at least 32 chars: stretched with SHA-256.
var placeholderKeys = map[string]bool{
	"changeme": true, "change-me": true, "change_me": true,
	"example": true, "placeholder": true, "replace-me": true,
	"your-secret-here": true, "your-key-here": true,
}

func isPlaceholderKey(raw string) bool {
	trimmed := strings.ToLower(strings.TrimSpace(raw))
	if strings.ContainsAny(trimmed, "<>") {
		return true
	}
	return placeholderKeys[trimmed]
}

func ParseKey(version int, raw string) (Key, error) {
	if version <= 0 {
		return Key{}, fmt.Errorf("credstore: key version must be >= 1, got %d", version)
	}
	if isPlaceholderKey(raw) {
		return Key{}, fmt.Errorf("credstore: key looks like a template placeholder, not a real secret; generate one with: openssl rand -hex 32")
	}
	if decoded, err := hex.DecodeString(raw); err == nil && len(decoded) == 32 {
		return Key{Version: version, aesKey: decoded}, nil
	}
	if len(raw) < 32 {
		return Key{}, fmt.Errorf("credstore: key must be 64 hex chars or a passphrase of at least 32 characters")
	}
	stretched := sha256.Sum256([]byte(raw))
	return Key{Version: version, aesKey: stretched[:]}, nil
}

// Store transforms credential payloads between plaintext (in memory) and
// their at-rest representation. PlainStore keeps files as-is for backwards
// compatibility; KeyedStore wraps them in authenticated envelopes.
type Store interface {
	// Encode converts plaintext bytes into their at-rest form.
	Encode(data []byte) ([]byte, error)
	// Decode converts at-rest bytes back into plaintext. Plaintext input is
	// passed through unchanged so mixed fleets migrate incrementally.
	Decode(data []byte) ([]byte, error)
	// Enabled reports whether the store encrypts at rest.
	Enabled() bool
}

// PlainStore is the no-op store used when no master key is configured.
type PlainStore struct{}

func (PlainStore) Encode(data []byte) ([]byte, error) { return data, nil }
func (PlainStore) Decode(data []byte) ([]byte, error) { return data, nil }
func (PlainStore) Enabled() bool                       { return false }

// KeyedStore encrypts with the current key and decrypts envelopes made with
// the current or the previous key (rotation window).
type KeyedStore struct {
	current  Key
	previous *Key
}

// NewKeyedStore builds a store. previous may be nil.
func NewKeyedStore(current Key, previous *Key) (*KeyedStore, error) {
	if len(current.aesKey) != 32 {
		return nil, fmt.Errorf("credstore: current key version %d is not AES-256 material", current.Version)
	}
	if previous != nil && len(previous.aesKey) != 32 {
		return nil, fmt.Errorf("credstore: previous key version %d is not AES-256 material", previous.Version)
	}
	return &KeyedStore{current: current, previous: previous}, nil
}

// CurrentVersion returns the active key version (for migration tooling).
func (s *KeyedStore) CurrentVersion() int { return s.current.Version }

type envelope struct {
	Format    int    `json:"cpvault"`
	KeyVer    int    `json:"kv"`
	Nonce     string `json:"nonce"`
	CipherTxt string `json:"ct"`
}

// IsEncrypted reports whether the payload is a vault envelope.
func IsEncrypted(data []byte) bool {
	var probe envelope
	if err := json.Unmarshal(data, &probe); err != nil {
		return false
	}
	return probe.Format == 1 && probe.KeyVer > 0 && probe.Nonce != "" && probe.CipherTxt != ""
}

func (s *KeyedStore) Encode(data []byte) ([]byte, error) {
	block, err := aes.NewCipher(s.current.aesKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	nonce := make([]byte, gcm.NonceSize())
	if _, err := io.ReadFull(rand.Reader, nonce); err != nil {
		return nil, err
	}
	sealed := gcm.Seal(nil, nonce, data, []byte(aadPrefix))
	out := envelope{
		Format:    1,
		KeyVer:    s.current.Version,
		Nonce:     base64.StdEncoding.EncodeToString(nonce),
		CipherTxt: base64.StdEncoding.EncodeToString(sealed),
	}
	return json.Marshal(out)
}

func (s *KeyedStore) Decode(data []byte) ([]byte, error) {
	if !IsEncrypted(data) {
		return data, nil
	}
	var env envelope
	if err := json.Unmarshal(data, &env); err != nil {
		return nil, fmt.Errorf("credstore: malformed envelope: %w", err)
	}
	key := s.keyFor(env.KeyVer)
	if key == nil {
		return nil, fmt.Errorf("%w (envelope kv=%d, active kv=%d)", ErrUnknownKeyVersion, env.KeyVer, s.current.Version)
	}
	nonce, err := base64.StdEncoding.DecodeString(env.Nonce)
	if err != nil {
		return nil, fmt.Errorf("credstore: bad nonce: %w", err)
	}
	sealed, err := base64.StdEncoding.DecodeString(env.CipherTxt)
	if err != nil {
		return nil, fmt.Errorf("credstore: bad ciphertext: %w", err)
	}
	block, err := aes.NewCipher(key.aesKey)
	if err != nil {
		return nil, err
	}
	gcm, err := cipher.NewGCM(block)
	if err != nil {
		return nil, err
	}
	if len(nonce) != gcm.NonceSize() {
		return nil, fmt.Errorf("credstore: nonce length %d, want %d", len(nonce), gcm.NonceSize())
	}
	plaintext, err := gcm.Open(nil, nonce, sealed, []byte(aadPrefix))
	if err != nil {
		return nil, fmt.Errorf("credstore: authentication failed (wrong key or tampered file): %w", err)
	}
	return plaintext, nil
}

func (s *KeyedStore) Enabled() bool { return true }

func (s *KeyedStore) keyFor(version int) *Key {
	if s.current.Version == version {
		return &s.current
	}
	if s.previous != nil && s.previous.Version == version {
		return s.previous
	}
	return nil
}

// ReadFile reads a credential file and decodes it if encrypted.
func ReadFile(store Store, path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return nil, err
	}
	return store.Decode(raw)
}
