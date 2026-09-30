package main

import (
	"encoding/json"
	"errors"
	"go.etcd.io/bbolt"
	"os"
	"time"
)

func preserveAccountSecurityOnRestore(currentPath, stagedPath string) error {
	if _, err := os.Stat(currentPath); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	current, err := bbolt.Open(currentPath, 0o600, &bbolt.Options{ReadOnly: true, Timeout: 2 * time.Second})
	if err != nil {
		return err
	}
	defer current.Close()
	staged, err := bbolt.Open(stagedPath, 0o600, &bbolt.Options{Timeout: 2 * time.Second})
	if err != nil {
		return err
	}
	defer staged.Close()
	return current.View(func(source *bbolt.Tx) error {
		return staged.Update(func(target *bbolt.Tx) error {
			if sourceBucket := source.Bucket([]byte(bucketClientCredentials)); sourceBucket != nil {
				destination, err := target.CreateBucketIfNotExists([]byte(bucketClientCredentials))
				if err != nil {
					return err
				}
				if err := sourceBucket.ForEach(func(k, v []byte) error {
					raw := destination.Get(k)
					if raw == nil {
						return nil
					}
					var current, previous ClientCredential
					if err := json.Unmarshal(v, &current); err != nil {
						return err
					}
					if err := json.Unmarshal(raw, &previous); err != nil {
						return err
					}
					if current.ID != previous.ID || current.PrincipalID != previous.PrincipalID {
						return errors.New("backup client ownership conflicts with current authority")
					}
					previous.Policy = current.Policy
					previous.ExpiresAt = current.ExpiresAt
					previous.DownloadDigest = current.DownloadDigest
					previous.DownloadCiphertext = current.DownloadCiphertext
					if current.Status != "active" {
						previous.Status = current.Status
					}
					if current.ValidAfter.After(previous.ValidAfter) {
						previous.ValidAfter = current.ValidAfter
					}
					return putJSON(destination, string(k), previous)
				}); err != nil {
					return err
				}
			}
			for _, name := range []string{bucketContributionAttempts, bucketPassportAudit} {
				sourceBucket := source.Bucket([]byte(name))
				if sourceBucket == nil {
					continue
				}
				destination, err := target.CreateBucketIfNotExists([]byte(name))
				if err != nil {
					return err
				}
				if err := sourceBucket.ForEach(func(k, v []byte) error { return destination.Put(k, v) }); err != nil {
					return err
				}
			}
			if sourceBucket := source.Bucket([]byte(bucketAccountResources)); sourceBucket != nil {
				destination, err := target.CreateBucketIfNotExists([]byte(bucketAccountResources))
				if err != nil {
					return err
				}
				if err := sourceBucket.ForEach(func(k, v []byte) error {
					var current accountResource
					if err := json.Unmarshal(v, &current); err != nil {
						return err
					}
					if current.Version != 2 {
						return errors.New("current account resources must be migrated before restore")
					}
					if _, err := readAccountResource(sourceBucket, current.Provider, current.ID); err != nil {
						return err
					}
					if raw := destination.Get(k); raw != nil {
						var previous accountResource
						if err := json.Unmarshal(raw, &previous); err != nil {
							return err
						}
						if previous.OwnerID != current.OwnerID || previous.Identity != current.Identity || previous.OperatorManaged != current.OperatorManaged {
							return errors.New("backup account ownership conflicts with current authority")
						}
						if previous.Revision > current.Revision && current.WithdrawnAt == nil {
							return nil
						}
					}
					return putJSON(destination, string(k), current)
				}); err != nil {
					return err
				}
			}
			if sourceBucket := source.Bucket([]byte(bucketPassportPolicyUsage)); sourceBucket != nil {
				destination, err := target.CreateBucketIfNotExists([]byte(bucketPassportPolicyUsage))
				if err != nil {
					return err
				}
				if err := sourceBucket.ForEach(func(k, v []byte) error {
					var counter policyUsageCounter
					if err := json.Unmarshal(v, &counter); err != nil {
						return err
					}
					previous, err := readPolicyCounter(destination, string(k))
					if err != nil {
						return err
					}
					if counter.Requests > previous.Requests {
						previous.Requests = counter.Requests
					}
					if counter.Tokens > previous.Tokens {
						previous.Tokens = counter.Tokens
					}
					if counter.ReservedTokens > previous.ReservedTokens {
						previous.ReservedTokens = counter.ReservedTokens
					}
					return writePolicyCounter(destination, string(k), previous)
				}); err != nil {
					return err
				}
			}
			if sourceBucket := source.Bucket([]byte(bucketPrincipals)); sourceBucket != nil {
				destination := target.Bucket([]byte(bucketPrincipals))
				if destination == nil {
					return errors.New("backup principal authority missing")
				}
				if err := sourceBucket.ForEach(func(k, v []byte) error {
					raw := destination.Get(k)
					if raw == nil {
						return nil
					}
					var current, previous Principal
					if err := json.Unmarshal(v, &current); err != nil {
						return err
					}
					if err := json.Unmarshal(raw, &previous); err != nil {
						return err
					}
					previous.Budget = current.Budget
					previous.CanContribute = current.CanContribute
					if previous.Kind != current.Kind {
						return errors.New("backup principal authority conflicts with current authority")
					}
					previous.ExpiresAt = current.ExpiresAt
					if current.PasswordChangedAt.After(previous.PasswordChangedAt) {
						previous.PasswordHash = current.PasswordHash
						previous.PasswordChangedAt = current.PasswordChangedAt
					}
					if current.Status == PrincipalSuspended {
						previous.Status = current.Status
					}
					if current.CredentialsValidAfter.After(previous.CredentialsValidAfter) {
						previous.CredentialsValidAfter = current.CredentialsValidAfter
					}
					return putJSON(destination, string(k), previous)
				}); err != nil {
					return err
				}
			}
			if state := source.Bucket([]byte(bucketAnalyticsState)); state != nil {
				destination, err := target.CreateBucketIfNotExists([]byte(bucketAnalyticsState))
				if err != nil {
					return err
				}
				for _, key := range []string{"account_ownership_migrated", "principal_policy_usage_migrated"} {
					if value := state.Get([]byte(key)); value != nil {
						if err := destination.Put([]byte(key), value); err != nil {
							return err
						}
					}
				}
			}
			return nil
		})
	})
}
