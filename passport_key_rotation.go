package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"time"

	"go.etcd.io/bbolt"
)

// rotatePassportKey re-seals every client download token in one Bolt transaction.
// The server must be stopped because Bolt has a single writer and the process
// keeps the old AEAD in memory until restart.
func rotatePassportKey(dbPath, oldSecret, newSecret string) (int, error) {
	if oldSecret == "" || len(newSecret) < 32 || oldSecret == newSecret {
		return 0, errors.New("distinct POOL_AUTH_ENCRYPTION_KEY_OLD and POOL_AUTH_ENCRYPTION_KEY (at least 32 characters) are required")
	}
	info, err := os.Stat(dbPath)
	if err != nil {
		return 0, fmt.Errorf("open existing Passport database: %w", err)
	}
	if !info.Mode().IsRegular() {
		return 0, errors.New("Passport database path is not a regular file")
	}
	oldAEAD, err := passportAEADForSecret(oldSecret)
	if err != nil {
		return 0, err
	}
	newAEAD, err := passportAEADForSecret(newSecret)
	if err != nil {
		return 0, err
	}
	db, err := bbolt.Open(dbPath, 0o600, &bbolt.Options{Timeout: time.Second})
	if err != nil {
		return 0, fmt.Errorf("open Passport database (stop the server first): %w", err)
	}
	defer db.Close()
	oldStore := &PassportStore{aead: oldAEAD}
	newStore := &PassportStore{aead: newAEAD}
	count := 0
	err = db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket([]byte(bucketClientCredentials))
		if bucket == nil {
			return errors.New("client credential bucket is missing")
		}
		type update struct {
			key   []byte
			value []byte
		}
		var updates []update
		if err := bucket.ForEach(func(key, value []byte) error {
			var client ClientCredential
			if err := json.Unmarshal(value, &client); err != nil {
				return fmt.Errorf("decode client credential: %w", err)
			}
			if client.ID == "" || client.ID != string(key) || client.PrincipalID == "" || len(client.DownloadCiphertext) == 0 {
				return fmt.Errorf("client credential %q has incomplete sealed data", key)
			}
			plaintext, err := oldStore.open("client", client.ID, client.PrincipalID, client.DownloadCiphertext)
			if err != nil {
				return fmt.Errorf("decrypt client credential %q: %w", key, err)
			}
			ciphertext, err := newStore.seal("client", client.ID, client.PrincipalID, plaintext)
			if err != nil {
				return fmt.Errorf("encrypt client credential %q: %w", key, err)
			}
			var record map[string]json.RawMessage
			if err := json.Unmarshal(value, &record); err != nil {
				return err
			}
			record["download_ciphertext"], err = json.Marshal(ciphertext)
			if err != nil {
				return err
			}
			encoded, err := json.Marshal(record)
			if err != nil {
				return err
			}
			updates = append(updates, update{key: append([]byte(nil), key...), value: encoded})
			return nil
		}); err != nil {
			return err
		}
		for _, item := range updates {
			if err := bucket.Put(item.key, item.value); err != nil {
				return err
			}
		}
		count = len(updates)
		return nil
	})
	return count, err
}
