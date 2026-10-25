package main

import (
	"bytes"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"time"

	"go.etcd.io/bbolt"
)

const bucketAuthBans = "auth_bans"

// passportBanStore persists one brute-force tracker's active bans in the
// Passport database. Keys are HMACs under the pool's stable analytics salt,
// so no client address or login is ever written to disk.
type passportBanStore struct {
	passport  *PassportStore
	namespace string
}

func (s passportBanStore) Key(client string) string {
	mac := hmac.New(sha256.New, []byte(s.passport.analyticsSalt))
	mac.Write([]byte(s.namespace + "|" + client))
	return s.namespace + "|" + hex.EncodeToString(mac.Sum(nil))
}

func (s passportBanStore) SaveBan(key string, bannedAt time.Time) error {
	return s.passport.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket([]byte(bucketAuthBans))
		if bucket == nil {
			return errors.New("auth bans unavailable")
		}
		return bucket.Put([]byte(key), []byte(bannedAt.UTC().Format(time.RFC3339Nano)))
	})
}

func (s passportBanStore) DeleteBans(keys []string) error {
	return s.passport.db.Update(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket([]byte(bucketAuthBans))
		if bucket == nil {
			return errors.New("auth bans unavailable")
		}
		for _, key := range keys {
			if err := bucket.Delete([]byte(key)); err != nil {
				return err
			}
		}
		return nil
	})
}

// LoadBans returns this namespace's bans. An unreadable timestamp loads as
// the zero time, which the tracker treats as expired and deletes.
func (s passportBanStore) LoadBans() (map[string]time.Time, error) {
	bans := map[string]time.Time{}
	prefix := []byte(s.namespace + "|")
	err := s.passport.db.View(func(tx *bbolt.Tx) error {
		bucket := tx.Bucket([]byte(bucketAuthBans))
		if bucket == nil {
			return errors.New("auth bans unavailable")
		}
		cursor := bucket.Cursor()
		for k, v := cursor.Seek(prefix); k != nil && bytes.HasPrefix(k, prefix); k, v = cursor.Next() {
			bannedAt, _ := time.Parse(time.RFC3339Nano, string(v))
			bans[string(k)] = bannedAt
		}
		return nil
	})
	return bans, err
}
