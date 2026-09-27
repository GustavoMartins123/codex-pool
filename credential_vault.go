package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"codex-pool-proxy/internal/credstore"
)

// accountCredentialStore encrypts upstream credential files
// (pool/<provider>/<account>.json) at rest when a master key is configured.
// Defaults to plaintext for compatibility with existing pools.
var accountCredentialStore credstore.Store = credstore.PlainStore{}

// buildCredentialStore reads POOL_CREDENTIAL_KEY (plus the _PREVIOUS
// rotation window) from the environment. Returns the plain store when no key
// is configured.
func buildCredentialStore() (credstore.Store, error) {
	raw := strings.TrimSpace(os.Getenv("POOL_CREDENTIAL_KEY"))
	if raw == "" {
		return credstore.PlainStore{}, nil
	}
	version := 1
	if v := strings.TrimSpace(os.Getenv("POOL_CREDENTIAL_KEY_VERSION")); v != "" {
		n, err := strconv.Atoi(v)
		if err != nil || n <= 0 {
			return nil, fmt.Errorf("POOL_CREDENTIAL_KEY_VERSION must be a positive integer")
		}
		version = n
	}
	current, err := credstore.ParseKey(version, raw)
	if err != nil {
		return nil, err
	}
	var previous *credstore.Key
	if prevRaw := strings.TrimSpace(os.Getenv("POOL_CREDENTIAL_KEY_PREVIOUS")); prevRaw != "" {
		prevVersion := version - 1
		if v := strings.TrimSpace(os.Getenv("POOL_CREDENTIAL_KEY_PREVIOUS_VERSION")); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				return nil, fmt.Errorf("POOL_CREDENTIAL_KEY_PREVIOUS_VERSION must be a positive integer")
			}
			prevVersion = n
		}
		prev, err := credstore.ParseKey(prevVersion, prevRaw)
		if err != nil {
			return nil, err
		}
		previous = &prev
	}
	return credstore.NewKeyedStore(current, previous)
}

// readAccountFile reads a credential file and decodes it through the vault.
func readAccountFile(path string) ([]byte, error) {
	return credstore.ReadFile(accountCredentialStore, path)
}

// writeFileAtomic replaces path with payload via a temp file in the same
// directory plus rename, so a crash mid-write can never truncate an existing
// credential file. The payload is written verbatim (already at-rest form).
func writeFileAtomic(path string, payload []byte) error {
	dir := filepath.Dir(path)
	tmp, err := os.CreateTemp(dir, "*.tmp")
	if err != nil {
		return err
	}
	tmpName := tmp.Name()
	defer os.Remove(tmpName)

	if err := tmp.Chmod(0o600); err != nil {
		tmp.Close()
		return err
	}
	if _, err := tmp.Write(payload); err != nil {
		tmp.Close()
		return err
	}
	if err := tmp.Close(); err != nil {
		return err
	}
	return os.Rename(tmpName, path)
}

// writeAccountFile encodes plaintext JSON into its at-rest form and writes it
// atomically with owner-only permissions. Every path that persists an account
// credential file must go through this (or atomicWriteJSON) so the vault
// cannot be bypassed by a handler that forgets to encode.
func writeAccountFile(path string, plaintext []byte) error {
	payload, err := accountCredentialStore.Encode(plaintext)
	if err != nil {
		return fmt.Errorf("encode credential file %s: %w", path, err)
	}
	return writeFileAtomic(path, payload)
}

// migrateCredentialFiles brings every account file in the pool directory to
// the current key state: plaintext files get encrypted and envelopes under
// the previous key get re-encrypted. Returns files encrypted and rotated.
func migrateCredentialFiles(poolDir string) (int, int, error) {
	keyed, ok := accountCredentialStore.(*credstore.KeyedStore)
	if !ok {
		return 0, 0, nil
	}
	encrypted, rotated := 0, 0
	err := filepath.Walk(poolDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(info.Name(), ".json") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return fmt.Errorf("read %s: %w", path, err)
		}
		if !credstore.IsEncrypted(raw) {
			atRest, err := keyed.Encode(raw)
			if err != nil {
				return fmt.Errorf("encrypt %s: %w", path, err)
			}
			if err := writeFileAtomic(path, atRest); err != nil {
				return fmt.Errorf("write %s: %w", path, err)
			}
			encrypted++
			return nil
		}
		var probe struct {
			KV int `json:"kv"`
		}
		if json.Unmarshal(raw, &probe) != nil || probe.KV == keyed.CurrentVersion() {
			return nil
		}
		plain, err := keyed.Decode(raw)
		if err != nil {
			return fmt.Errorf("rotate %s: %w", path, err)
		}
		atRest, err := keyed.Encode(plain)
		if err != nil {
			return fmt.Errorf("re-encrypt %s: %w", path, err)
		}
		if err := writeFileAtomic(path, atRest); err != nil {
			return fmt.Errorf("write %s: %w", path, err)
		}
		rotated++
		return nil
	})
	return encrypted, rotated, err
}

// decryptCredentialFiles reverts every encrypted account file to plaintext.
// Rollback path for the -decrypt-credentials flag; requires the master key.
func decryptCredentialFiles(poolDir string) (int, error) {
	keyed, ok := accountCredentialStore.(*credstore.KeyedStore)
	if !ok {
		return 0, fmt.Errorf("credential decryption requires POOL_CREDENTIAL_KEY")
	}
	count := 0
	err := filepath.Walk(poolDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(info.Name(), ".json") {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			return err
		}
		if !credstore.IsEncrypted(raw) {
			return nil
		}
		plain, err := keyed.Decode(raw)
		if err != nil {
			return fmt.Errorf("decrypt %s: %w", path, err)
		}
		if err := writeFileAtomic(path, plain); err != nil {
			return err
		}
		count++
		return nil
	})
	return count, err
}

// ensureNoEncryptedFilesWithoutKey fails startup when encrypted credential
// files exist but no master key is configured: silently skipping them would
// present the pool as empty.
func ensureNoEncryptedFilesWithoutKey(poolDir string) error {
	if accountCredentialStore.Enabled() {
		return nil
	}
	var offenders []string
	_ = filepath.Walk(poolDir, func(path string, info os.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(info.Name(), ".json") {
			return nil
		}
		if raw, err := os.ReadFile(path); err == nil && credstore.IsEncrypted(raw) {
			offenders = append(offenders, path)
		}
		return nil
	})
	if len(offenders) > 0 {
		return fmt.Errorf("%d credential file(s) are encrypted but POOL_CREDENTIAL_KEY is not set (e.g. %s)", len(offenders), offenders[0])
	}
	return nil
}

// initCredentialVault wires the global store, migrates the pool directory,
// and logs the outcome. Called before the pool is first loaded.
func initCredentialVault(poolDir string) error {
	store, err := buildCredentialStore()
	if err != nil {
		return err
	}
	accountCredentialStore = store
	if !store.Enabled() {
		return ensureNoEncryptedFilesWithoutKey(poolDir)
	}
	encrypted, rotated, err := migrateCredentialFiles(poolDir)
	if err != nil {
		return err
	}
	if encrypted > 0 || rotated > 0 {
		log.Printf("credential vault: encrypted %d file(s), rotated %d file(s) to the current key", encrypted, rotated)
	} else {
		log.Printf("credential vault: all credential files already encrypted at the current key version")
	}
	return nil
}
