package main

import (
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"time"

	_ "github.com/duckdb/duckdb-go/v2"
	"go.etcd.io/bbolt"
)

const passportBackupFormat = 1

type backupFileManifest struct {
	Name   string `json:"name"`
	SHA256 string `json:"sha256"`
	Bytes  int64  `json:"bytes"`
}

type passportBackupManifest struct {
	Format    int                `json:"format"`
	CreatedAt time.Time          `json:"created_at"`
	Bolt      backupFileManifest `json:"bolt"`
	DuckDB    backupFileManifest `json:"duckdb"`
}

func fileManifest(path string) (backupFileManifest, error) {
	file, err := os.Open(path)
	if err != nil {
		return backupFileManifest{}, err
	}
	defer file.Close()
	hash := sha256.New()
	bytes, err := io.Copy(hash, file)
	if err != nil {
		return backupFileManifest{}, err
	}
	return backupFileManifest{Name: filepath.Base(path), SHA256: hex.EncodeToString(hash.Sum(nil)), Bytes: bytes}, nil
}

func copyFile(source, destination string, mode os.FileMode) error {
	input, err := os.Open(source)
	if err != nil {
		return err
	}
	defer input.Close()
	output, err := os.OpenFile(destination, os.O_CREATE|os.O_EXCL|os.O_WRONLY, mode)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = output.Close()
		if !ok {
			_ = os.Remove(destination)
		}
	}()
	if _, err := io.Copy(output, input); err != nil {
		return err
	}
	if err := output.Sync(); err != nil {
		return err
	}
	ok = true
	return output.Close()
}

func createPairedBackup(boltPath, duckPath, directory string) (string, error) {
	if directory == "" {
		return "", errors.New("backup directory required")
	}
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", err
	}
	stamp := time.Now().UTC().Format("20060102T150405Z")
	boltBackup := filepath.Join(directory, "proxy-"+stamp+".db")
	duckBackup := filepath.Join(directory, "usage-"+stamp+".duckdb")

	bolt, err := bbolt.Open(boltPath, 0o600, &bbolt.Options{ReadOnly: true, Timeout: 2 * time.Second})
	if err != nil {
		return "", fmt.Errorf("open Bolt read-only (stop the service before backup): %w", err)
	}
	if err := bolt.View(func(tx *bbolt.Tx) error { return tx.CopyFile(boltBackup, 0o600) }); err != nil {
		bolt.Close()
		return "", fmt.Errorf("copy Bolt: %w", err)
	}
	if err := bolt.Close(); err != nil {
		return "", err
	}

	duck, err := sql.Open("duckdb", duckPath)
	if err != nil {
		return "", err
	}
	if _, err := duck.Exec("CHECKPOINT"); err != nil {
		duck.Close()
		return "", fmt.Errorf("checkpoint DuckDB (stop the service before backup): %w", err)
	}
	if err := duck.Close(); err != nil {
		return "", err
	}
	if err := copyFile(duckPath, duckBackup, 0o600); err != nil {
		return "", fmt.Errorf("copy DuckDB: %w", err)
	}

	boltFile, err := fileManifest(boltBackup)
	if err != nil {
		return "", err
	}
	duckFile, err := fileManifest(duckBackup)
	if err != nil {
		return "", err
	}
	manifest := passportBackupManifest{Format: passportBackupFormat, CreatedAt: time.Now().UTC(), Bolt: boltFile, DuckDB: duckFile}
	encoded, err := json.MarshalIndent(manifest, "", "  ")
	if err != nil {
		return "", err
	}
	manifestPath := filepath.Join(directory, "passport-backup-"+stamp+".json")
	if err := os.WriteFile(manifestPath, append(encoded, '\n'), 0o600); err != nil {
		return "", err
	}
	return manifestPath, nil
}

func verifyBackupFile(path string, expected backupFileManifest) error {
	actual, err := fileManifest(path)
	if err != nil {
		return err
	}
	if actual.Bytes != expected.Bytes || actual.SHA256 != expected.SHA256 {
		return fmt.Errorf("backup checksum mismatch for %s", expected.Name)
	}
	return nil
}

// restoreRename is indirected so tests can deterministically fail the
// first or second swap and prove the rollback restores both stores.
var restoreRename = os.Rename

func restorePairedBackup(manifestPath, boltPath, duckPath string) error {
	encoded, err := os.ReadFile(manifestPath)
	if err != nil {
		return err
	}
	var manifest passportBackupManifest
	if json.Unmarshal(encoded, &manifest) != nil || manifest.Format != passportBackupFormat {
		return errors.New("unsupported backup manifest")
	}
	directory := filepath.Dir(manifestPath)
	boltBackup := filepath.Join(directory, manifest.Bolt.Name)
	duckBackup := filepath.Join(directory, manifest.DuckDB.Name)
	if err := verifyBackupFile(boltBackup, manifest.Bolt); err != nil {
		return err
	}
	if err := verifyBackupFile(duckBackup, manifest.DuckDB); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(boltPath), 0o700); err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(duckPath), 0o700); err != nil {
		return err
	}
	boltTemp, duckTemp := boltPath+".restore", duckPath+".restore"
	_ = os.Remove(boltTemp)
	_ = os.Remove(duckTemp)
	if err := copyFile(boltBackup, boltTemp, 0o600); err != nil {
		return err
	}
	if err := copyFile(duckBackup, duckTemp, 0o600); err != nil {
		_ = os.Remove(boltTemp)
		return err
	}

	// The paired invariant: both stores move to the backup snapshot, or
	// neither does. Before the first swap, keep aside COPIES of the current
	// stores (copies, not renames: the originals must stay live until each
	// swap succeeds so a mid-restore crash never leaves both stores
	// missing). If the DuckDB swap fails after the Bolt swap succeeded, the
	// aside copy rolls Bolt back to its pre-restore state.
	//
	// Crash-atomicity limits, stated precisely: this rollback covers
	// RETURNED errors, not power loss. A process death between the Bolt and
	// DuckDB swaps still leaves the pair inconsistent (Bolt on the backup
	// snapshot, DuckDB current); the surviving .prerestore copies enable
	// manual recovery, but no two-file rename sequence can be atomic across
	// a crash. A journaling restart-recovery protocol would be needed for
	// that; until then, run restores with the service stopped and verify
	// startup logs afterwards.
	if err := preserveAccountSecurityOnRestore(boltPath, boltTemp); err != nil {
		_ = os.Remove(boltTemp)
		_ = os.Remove(duckTemp)
		return fmt.Errorf("preserve account security: %w", err)
	}
	boltAside, duckAside := boltPath+".prerestore", duckPath+".prerestore"
	_ = os.Remove(boltAside)
	_ = os.Remove(duckAside)
	boltHadStore := true
	if err := copyFile(boltPath, boltAside, 0o600); err != nil {
		if !os.IsNotExist(err) {
			_ = os.Remove(boltTemp)
			_ = os.Remove(duckTemp)
			return fmt.Errorf("snapshot current Bolt store before restore: %w", err)
		}
		boltHadStore = false
	}
	if err := copyFile(duckPath, duckAside, 0o600); err != nil {
		if !os.IsNotExist(err) {
			_ = os.Remove(boltTemp)
			_ = os.Remove(duckTemp)
			_ = os.Remove(boltAside)
			return fmt.Errorf("snapshot current DuckDB store before restore: %w", err)
		}
	}
	cleanupStaging := func() {
		_ = os.Remove(boltTemp)
		_ = os.Remove(duckTemp)
		_ = os.Remove(boltAside)
		_ = os.Remove(duckAside)
	}

	if err := restoreRename(boltTemp, boltPath); err != nil {
		cleanupStaging()
		return fmt.Errorf("restore Bolt store: %w", err)
	}
	if err := restoreRename(duckTemp, duckPath); err != nil {
		// Roll the Bolt swap back so the stores stay a matched pair.
		if boltHadStore {
			if rollbackErr := restoreRename(boltAside, boltPath); rollbackErr != nil {
				return fmt.Errorf("restore DuckDB store failed (%v) AND rolling back the Bolt swap failed (%v); "+
					"the previous Bolt store is preserved at %s — restore it manually before restarting", err, rollbackErr, boltAside)
			}
		} else {
			_ = os.Remove(boltPath)
		}
		cleanupStaging()
		return fmt.Errorf("restore DuckDB store: %w (Bolt rolled back to its pre-restore state)", err)
	}
	cleanupStaging()
	return nil
}
