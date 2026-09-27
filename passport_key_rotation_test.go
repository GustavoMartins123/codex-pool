package main

import (
	"encoding/json"
	"path/filepath"
	"testing"

	"go.etcd.io/bbolt"
)

func TestRotatePassportKeyIsAtomic(t *testing.T) {
	const oldSecret = "old-development-passport-key"
	const newSecret = "new-passport-key-with-at-least-32-chars"
	oldAEAD, err := passportAEADForSecret(oldSecret)
	if err != nil {
		t.Fatal(err)
	}
	oldStore := &PassportStore{aead: oldAEAD}
	path := filepath.Join(t.TempDir(), "proxy.db")
	db, err := bbolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	first, err := oldStore.seal("client", "first", "principal", "download-token-one")
	if err != nil {
		t.Fatal(err)
	}
	second, err := oldStore.seal("client", "second", "principal", "download-token-two")
	if err != nil {
		t.Fatal(err)
	}
	put := func(id string, ciphertext []byte) {
		t.Helper()
		client := ClientCredential{ID: id, PrincipalID: "principal", DownloadCiphertext: ciphertext}
		value, err := json.Marshal(client)
		if err != nil {
			t.Fatal(err)
		}
		if err := db.Update(func(tx *bbolt.Tx) error {
			bucket, err := tx.CreateBucketIfNotExists([]byte(bucketClientCredentials))
			if err != nil {
				return err
			}
			return bucket.Put([]byte(id), value)
		}); err != nil {
			t.Fatal(err)
		}
	}
	put("first", first)
	put("second", []byte("corrupt"))
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := rotatePassportKey(path, oldSecret, newSecret); err == nil {
		t.Fatal("rotation accepted a corrupt record")
	}
	db, err = bbolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := db.View(func(tx *bbolt.Tx) error {
		var client ClientCredential
		if err := json.Unmarshal(tx.Bucket([]byte(bucketClientCredentials)).Get([]byte("first")), &client); err != nil {
			return err
		}
		if _, err := oldStore.open("client", client.ID, client.PrincipalID, client.DownloadCiphertext); err != nil {
			t.Fatal("failed rotation changed the first record:", err)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = bbolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	put("second", second)
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	count, err := rotatePassportKey(path, oldSecret, newSecret)
	if err != nil || count != 2 {
		t.Fatalf("rotate count=%d err=%v", count, err)
	}
	newAEAD, err := passportAEADForSecret(newSecret)
	if err != nil {
		t.Fatal(err)
	}
	newStore := &PassportStore{aead: newAEAD}
	db, err = bbolt.Open(path, 0o600, nil)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	if err := db.View(func(tx *bbolt.Tx) error {
		for id, expected := range map[string]string{"first": "download-token-one", "second": "download-token-two"} {
			var client ClientCredential
			if err := json.Unmarshal(tx.Bucket([]byte(bucketClientCredentials)).Get([]byte(id)), &client); err != nil {
				return err
			}
			got, err := newStore.open("client", client.ID, client.PrincipalID, client.DownloadCiphertext)
			if err != nil || got != expected {
				t.Fatalf("rotated %s: token=%q err=%v", id, got, err)
			}
			if _, err := oldStore.open("client", client.ID, client.PrincipalID, client.DownloadCiphertext); err == nil {
				t.Fatalf("old key still decrypts %s", id)
			}
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}
