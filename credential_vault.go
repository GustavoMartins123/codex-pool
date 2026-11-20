package main

import (
	"encoding/json"
	"fmt"
	"log"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"codex-pool-proxy/internal/atomicfile"
	"codex-pool-proxy/internal/credstore"
)

// credentialVault owns the store used by this application's account files.
type credentialVault struct {
	store credstore.Store
}

// buildCredentialStore requires a master key and accepts an explicitly configured rotation key.
func buildCredentialStore() (*credstore.KeyedStore, error) {
	raw := strings.TrimSpace(os.Getenv("POOL_CREDENTIAL_KEY"))
	if raw == "" {
		return nil, fmt.Errorf("POOL_CREDENTIAL_KEY is required")
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
func (v credentialVault) readAccountFile(path string) ([]byte, error) {
	return credstore.ReadFile(v.store, path)
}

// renameRetryDelays bounds how long an atomic replace waits for transient
// destination locks. Windows is the only platform with such locks (see
// rename_retry_windows.go); the total budget stays small because writers run
// inside request paths (token refresh, admin saves).
var renameRetryDelays = []time.Duration{
	25 * time.Millisecond,
	50 * time.Millisecond,
	100 * time.Millisecond,
	200 * time.Millisecond,
	400 * time.Millisecond,
	800 * time.Millisecond,
}

// writeFileAtomicRetryHook, when non-nil, fires after each failed rename
// attempt. Tests use it to coordinate releasing a simulated lock exactly
// after the writer has observed it.
var writeFileAtomicRetryHook func()

// renameWithRetry replaces oldPath with newPath, retrying transient
// destination locks (Windows only) with a bounded backoff. Permanent locks
// still fail, after at most the total retry budget.
func renameWithRetry(oldPath, newPath string) error {
	var err error
	for attempt := 0; ; attempt++ {
		err = atomicfile.Replace(oldPath, newPath)
		if err == nil || !atomicfile.IsTransientRenameError(err) || attempt >= len(renameRetryDelays) {
			return err
		}
		if writeFileAtomicRetryHook != nil {
			writeFileAtomicRetryHook()
		}
		time.Sleep(renameRetryDelays[attempt])
	}
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
	return renameWithRetry(tmpName, path)
}

// writeAccountFile encodes plaintext JSON into its at-rest form and writes it
// atomically with owner-only permissions. Every path that persists an account
// credential file must go through this (or atomicWriteJSON) so the vault
// cannot be bypassed by a handler that forgets to encode.
func (v credentialVault) writeAccountFile(path string, plaintext []byte) error {
	if v.store == nil {
		return fmt.Errorf("credential store is not initialized")
	}
	payload, err := v.store.Encode(plaintext)
	if err != nil {
		return fmt.Errorf("encode credential file %s: %w", path, err)
	}
	return writeFileAtomic(path, payload)
}

func (v credentialVault) atomicWriteJSON(path string, data any) error {
	updated, err := json.MarshalIndent(data, "", "  ")
	if err != nil {
		return err
	}
	return v.writeAccountFile(path, updated)
}

// migrateCredentialFiles brings every account file in the pool directory to
// the current key state: plaintext files get encrypted and envelopes under
// the previous key get re-encrypted. Returns files encrypted and rotated.
func (v credentialVault) migrateCredentialFiles(poolDir string) (int, int, error) {
	keyed, ok := v.store.(*credstore.KeyedStore)
	if !ok || keyed == nil {
		return 0, 0, fmt.Errorf("credential migration requires a keyed store")
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
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(raw, &fields); err != nil || fields == nil {
				return fmt.Errorf("invalid credential JSON %s", path)
			}
			if _, exists := fields["cpvault"]; exists {
				return fmt.Errorf("malformed credential envelope %s", path)
			}
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
		plain, err := keyed.Decode(raw)
		if err != nil {
			return fmt.Errorf("authenticate %s: %w", path, err)
		}
		if err := json.Unmarshal(raw, &probe); err != nil {
			return fmt.Errorf("inspect %s: %w", path, err)
		}
		if probe.KV == keyed.CurrentVersion() {
			return nil
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
func (v credentialVault) decryptCredentialFiles(poolDir string) (int, error) {
	keyed, ok := v.store.(*credstore.KeyedStore)
	if !ok || keyed == nil {
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

type credentialCheckStats struct {
	Checked   int
	Current   int
	Previous  int
	Plaintext int
	Failures  []string
}

type credentialEnvelopeProbe struct {
	KeyVer int `json:"kv"`
}

// verifyCredentialFiles decodes every credential file with the configured
// vault. Traversal errors fail the check; files are classified as current,
// previous-key (rotation window, not yet re-encrypted), plaintext, or
// failures. Used by the -check-credentials flag.
func (v credentialVault) verifyCredentialFiles(poolDir string) (credentialCheckStats, error) {
	var stats credentialCheckStats
	keyed, ok := v.store.(*credstore.KeyedStore)
	if !ok || keyed == nil {
		return stats, fmt.Errorf("credential verification requires a keyed store")
	}
	err := filepath.Walk(poolDir, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		if info.IsDir() || !strings.HasSuffix(info.Name(), ".json") {
			return nil
		}
		stats.Checked++
		raw, err := os.ReadFile(path)
		if err != nil {
			stats.Failures = append(stats.Failures, path+": "+err.Error())
			return nil
		}
		if !credstore.IsEncrypted(raw) {
			stats.Plaintext++
			stats.Failures = append(stats.Failures, path+": file is not encrypted; start the server once with POOL_CREDENTIAL_KEY to migrate it")
			return nil
		}
		if _, err := v.store.Decode(raw); err != nil {
			stats.Failures = append(stats.Failures, path+": "+err.Error())
			return nil
		}
		var probe credentialEnvelopeProbe
		if err := json.Unmarshal(raw, &probe); err != nil {
			return fmt.Errorf("inspect credential envelope %s: %w", path, err)
		}
		if probe.KeyVer != keyed.CurrentVersion() {
			stats.Previous++
			return nil
		}
		stats.Current++
		return nil
	})
	if err != nil {
		return stats, fmt.Errorf("walk %s: %w", poolDir, err)
	}
	return stats, nil
}

// newCredentialVault migrates the pool before the application loads credentials.
func newCredentialVault(poolDir string) (credentialVault, error) {
	store, err := buildCredentialStore()
	if err != nil {
		return credentialVault{}, err
	}
	vault := credentialVault{store: store}
	encrypted, rotated, err := vault.migrateCredentialFiles(poolDir)
	if err != nil {
		return credentialVault{}, err
	}
	if encrypted > 0 || rotated > 0 {
		log.Printf("credential vault: encrypted %d file(s), rotated %d file(s) to the current key", encrypted, rotated)
	} else {
		log.Printf("credential vault: all credential files already encrypted at the current key version")
	}
	return vault, nil
}
