package main

import (
	"encoding/json"
	"errors"
	"go.etcd.io/bbolt"
)

const bucketAccountGrantIndex = "account_grants_by_resource_recipient"

func readAccountGrant(b *bbolt.Bucket, id string) (*accountGrant, error) {
	if b == nil {
		return nil, errors.New("account grants unavailable")
	}
	raw := b.Get([]byte(id))
	if raw == nil {
		return nil, nil
	}
	var grant accountGrant
	if err := json.Unmarshal(raw, &grant); err != nil {
		return nil, err
	}
	if err := grant.validate(); err != nil {
		return nil, err
	}
	if grant.ID != id {
		return nil, errors.New("invalid grant key")
	}
	return &grant, nil
}

func indexAccountGrant(tx *bbolt.Tx, grant accountGrant) error {
	index := tx.Bucket([]byte(bucketAccountGrantIndex))
	if index == nil {
		return errors.New("account grant index unavailable")
	}
	resource, err := index.CreateBucketIfNotExists([]byte(resourceKey(grant.Provider, grant.AccountID)))
	if err != nil {
		return err
	}
	recipient, err := resource.CreateBucketIfNotExists([]byte(grant.RecipientID))
	if err != nil {
		return err
	}
	return recipient.Put([]byte(grant.ID), []byte{1})
}

func putAccountGrant(tx *bbolt.Tx, grant accountGrant) error {
	if err := grant.validate(); err != nil {
		return err
	}
	b := tx.Bucket([]byte(bucketAccountGrants))
	previous, err := readAccountGrant(b, grant.ID)
	if err != nil {
		return err
	}
	if previous != nil && (previous.Provider != grant.Provider || previous.AccountID != grant.AccountID || previous.RecipientID != grant.RecipientID) {
		return errors.New("grant index identity is immutable")
	}
	if err := putJSON(b, grant.ID, grant); err != nil {
		return err
	}
	return indexAccountGrant(tx, grant)
}

func initializeAccountGrantIndex(tx *bbolt.Tx) error {
	grants, err := readAccountGrants(tx.Bucket([]byte(bucketAccountGrants)))
	if err != nil {
		return err
	}
	if tx.Bucket([]byte(bucketAccountGrantIndex)) == nil {
		if _, err := tx.CreateBucket([]byte(bucketAccountGrantIndex)); err != nil {
			return err
		}
		for _, grant := range grants {
			if err := indexAccountGrant(tx, grant); err != nil {
				return err
			}
		}
	}
	index := tx.Bucket([]byte(bucketAccountGrantIndex))
	seen := map[string]bool{}
	err = index.ForEach(func(resourceID, value []byte) error {
		if value != nil {
			return errors.New("invalid grant resource index")
		}
		resource := index.Bucket(resourceID)
		return resource.ForEach(func(recipientKey, value []byte) error {
			if value != nil {
				return errors.New("invalid grant recipient index")
			}
			return resource.Bucket(recipientKey).ForEach(func(id, value []byte) error {
				grant, err := readAccountGrant(tx.Bucket([]byte(bucketAccountGrants)), string(id))
				if err != nil {
					return err
				}
				if grant == nil || string(resourceID) != resourceKey(grant.Provider, grant.AccountID) || string(recipientKey) != grant.RecipientID || string(value) != string([]byte{1}) || seen[grant.ID] {
					return errors.New("inconsistent grant index")
				}
				seen[grant.ID] = true
				return nil
			})
		})
	})
	if err != nil {
		return err
	}
	if len(seen) != len(grants) {
		return errors.New("incomplete grant index")
	}
	return nil
}

func accountGrantsByResource(tx *bbolt.Tx, provider AccountType, id, recipient string) ([]accountGrant, error) {
	index := tx.Bucket([]byte(bucketAccountGrantIndex))
	if index == nil {
		return nil, errors.New("account grant index unavailable")
	}
	resource := index.Bucket([]byte(resourceKey(provider, id)))
	if resource == nil {
		return []accountGrant{}, nil
	}
	grants := []accountGrant{}
	read := func(key []byte, b *bbolt.Bucket) error {
		return b.ForEach(func(k, v []byte) error {
			grant, err := readAccountGrant(tx.Bucket([]byte(bucketAccountGrants)), string(k))
			if err != nil {
				return err
			}
			if grant == nil || grant.Provider != provider || grant.AccountID != id || grant.RecipientID != string(key) || string(v) != string([]byte{1}) {
				return errors.New("inconsistent grant index")
			}
			grants = append(grants, *grant)
			return nil
		})
	}
	if recipient != "" {
		b := resource.Bucket([]byte(recipient))
		if b == nil {
			return grants, nil
		}
		err := read([]byte(recipient), b)
		return grants, err
	}
	err := resource.ForEach(func(k, v []byte) error {
		if v != nil {
			return errors.New("invalid grant index")
		}
		return read(k, resource.Bucket(k))
	})
	return grants, err
}
