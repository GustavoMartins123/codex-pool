package credstore

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func mustKey(t *testing.T, version int, raw string) Key {
	t.Helper()
	key, err := ParseKey(version, raw)
	if err != nil {
		t.Fatalf("ParseKey(v=%d): %v", version, err)
	}
	return key
}

func TestRoundTripHexAndPassphraseKeys(t *testing.T) {
	secret := []byte(`{"tokens":{"access_token":"super-secret-token"}}`)
	for _, raw := range []string{
		strings.Repeat("ab", 32), // 64 hex chars
		strings.Repeat("p", 40),  // passphrase
	} {
		store, err := NewKeyedStore(mustKey(t, 1, raw), nil)
		if err != nil {
			t.Fatalf("NewKeyedStore: %v", err)
		}
		atRest, err := store.Encode(secret)
		if err != nil {
			t.Fatalf("Encode: %v", err)
		}
		if bytes.Contains(atRest, secret) || bytes.Contains(atRest, []byte("super-secret-token")) {
			t.Fatalf("ciphertext leaked plaintext: %s", atRest)
		}
		if !IsEncrypted(atRest) {
			t.Fatalf("envelope not detected: %s", atRest)
		}
		back, err := store.Decode(atRest)
		if err != nil {
			t.Fatalf("Decode: %v", err)
		}
		if !bytes.Equal(back, secret) {
			t.Fatalf("roundtrip mismatch: %s", back)
		}
	}
}

func TestParseKeyRejectsWeakInput(t *testing.T) {
	if _, err := ParseKey(1, "short"); err == nil {
		t.Fatal("short passphrase must be rejected")
	}
	if _, err := ParseKey(0, strings.Repeat("ab", 32)); err == nil {
		t.Fatal("version 0 must be rejected")
	}
}

func TestDecodeWrongKeyFails(t *testing.T) {
	storeA, _ := NewKeyedStore(mustKey(t, 1, strings.Repeat("aa", 32)), nil)
	storeB, _ := NewKeyedStore(mustKey(t, 1, strings.Repeat("bb", 32)), nil)
	atRest, err := storeA.Encode([]byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	if _, err := storeB.Decode(atRest); err == nil {
		t.Fatal("decryption with the wrong key must fail")
	}
}

func TestDecodeTamperedCiphertextFails(t *testing.T) {
	store, _ := NewKeyedStore(mustKey(t, 1, strings.Repeat("cc", 32)), nil)
	atRest, err := store.Encode([]byte("payload"))
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(atRest, &env); err != nil {
		t.Fatal(err)
	}
	// Flip bytes inside the ciphertext body.
	decoded := []byte(env.CipherTxt)
	decoded[len(decoded)-2] ^= 0xff
	env.CipherTxt = string(decoded)
	tampered, _ := json.Marshal(env)
	if _, err := store.Decode(tampered); err == nil {
		t.Fatal("tampered ciphertext must fail authentication")
	}
}

func TestRotationPreviousKeyDecrypts(t *testing.T) {
	v1 := mustKey(t, 1, strings.Repeat("11", 32))
	v2 := mustKey(t, 2, strings.Repeat("22", 32))
	oldStore, _ := NewKeyedStore(v1, nil)
	atRest, err := oldStore.Encode([]byte(`{"api_key":"tk"}`))
	if err != nil {
		t.Fatal(err)
	}

	// Without the previous key, version 2 cannot read version 1 envelopes.
	rotatedStrict, _ := NewKeyedStore(v2, nil)
	if _, err := rotatedStrict.Decode(atRest); err == nil {
		t.Fatal("unknown key version must fail closed")
	}

	// With the previous key configured, decryption succeeds and re-encoding
	// upgrades to the current version.
	rotated, _ := NewKeyedStore(v2, &v1)
	back, err := rotated.Decode(atRest)
	if err != nil {
		t.Fatalf("rotation window must decrypt old envelopes: %v", err)
	}
	if string(back) != `{"api_key":"tk"}` {
		t.Fatalf("rotated decode mismatch: %s", back)
	}
	upgraded, err := rotated.Encode(back)
	if err != nil {
		t.Fatal(err)
	}
	var env envelope
	if err := json.Unmarshal(upgraded, &env); err != nil {
		t.Fatal(err)
	}
	if env.KeyVer != 2 {
		t.Fatalf("re-encode must use current version, got kv=%d", env.KeyVer)
	}
	if _, err := rotatedStrict.Decode(upgraded); err != nil {
		t.Fatalf("upgraded envelope must decode with current key only: %v", err)
	}
}

func TestPlaintextPassesThrough(t *testing.T) {
	store, _ := NewKeyedStore(mustKey(t, 1, strings.Repeat("dd", 32)), nil)
	plain := []byte(`{"tokens":{"access_token":"x"}}`)
	if IsEncrypted(plain) {
		t.Fatal("account JSON must not be misdetected as an envelope")
	}
	back, err := store.Decode(plain)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(back, plain) {
		t.Fatalf("plaintext must round-trip unchanged: %s", back)
	}
	var p PlainStore
	enc, _ := p.Encode(plain)
	if !bytes.Equal(enc, plain) {
		t.Fatal("PlainStore must be identity")
	}
	if p.Enabled() {
		t.Fatal("PlainStore must not report encryption enabled")
	}
}

func TestNonceUniqueness(t *testing.T) {
	store, _ := NewKeyedStore(mustKey(t, 1, strings.Repeat("ee", 32)), nil)
	first, _ := store.Encode([]byte("same input"))
	second, _ := store.Encode([]byte("same input"))
	if bytes.Equal(first, second) {
		t.Fatal("two encryptions of the same input must differ (nonce reuse)")
	}
}
