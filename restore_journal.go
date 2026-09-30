package main

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"codex-pool-proxy/internal/atomicfile"
)

type restoreJournalFile struct {
	Path     string              `json:"path"`
	Previous *backupFileManifest `json:"previous"`
	Next     backupFileManifest  `json:"next"`
}

type restoreJournal struct {
	Format    int                  `json:"format"`
	Committed bool                 `json:"committed"`
	Files     []restoreJournalFile `json:"files"`
}

var restoreCheckpoint = func(string) {}

func writeRestoreJournal(path string, journal restoreJournal) error {
	data, err := json.Marshal(journal)
	if err != nil {
		return err
	}
	temp := path + ".tmp"
	f, err := os.OpenFile(temp, os.O_CREATE|os.O_TRUNC|os.O_WRONLY, 0600)
	if err != nil {
		return err
	}
	if _, err = f.Write(data); err == nil {
		err = f.Sync()
	}
	closeErr := f.Close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	return atomicfile.Replace(temp, path)
}

func prepareRestoreJournal(boltPath, duckPath string) (restoreJournal, error) {
	j := restoreJournal{Format: 1}
	for _, path := range []string{boltPath, duckPath} {
		absolute, err := filepath.Abs(path)
		if err != nil {
			return j, err
		}
		next, err := fileManifest(path + ".restore")
		if err != nil {
			return j, err
		}
		entry := restoreJournalFile{Path: absolute, Next: next}
		previous, err := fileManifest(path + ".prerestore")
		if err == nil {
			entry.Previous = &previous
		} else if !os.IsNotExist(err) {
			return j, err
		}
		j.Files = append(j.Files, entry)
	}
	return j, nil
}

func recoverPairedRestore(boltPath, duckPath string) error {
	journalPath := boltPath + ".restore-journal"
	data, err := os.ReadFile(journalPath)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var j restoreJournal
	if json.Unmarshal(data, &j) != nil || j.Format != 1 || len(j.Files) != 2 {
		return errors.New("invalid restore journal")
	}
	for i, path := range []string{boltPath, duckPath} {
		absolute, err := filepath.Abs(path)
		if err != nil || absolute != j.Files[i].Path {
			return errors.New("restore journal path mismatch")
		}
		entry := j.Files[i]
		if j.Committed {
			if err := verifyBackupFile(path, entry.Next); err != nil {
				return err
			}
		} else if entry.Previous != nil {
			if err := verifyBackupFile(path+".prerestore", *entry.Previous); err != nil {
				return err
			}
		}
	}
	if !j.Committed {
		for _, entry := range j.Files {
			if entry.Previous == nil {
				if err := os.Remove(entry.Path); err != nil && !os.IsNotExist(err) {
					return err
				}
			} else {
				if verifyBackupFile(entry.Path, *entry.Previous) == nil {
					continue
				}
				// Keep the snapshot until both replacements finish.
				temp := entry.Path + ".recover"
				if err := os.Remove(temp); err != nil && !os.IsNotExist(err) {
					return err
				}
				if err := copyFile(entry.Path+".prerestore", temp, 0600); err != nil {
					return err
				}
				if err := atomicfile.Replace(temp, entry.Path); err != nil {
					return fmt.Errorf("recover store: %w", err)
				}
			}
			restoreCheckpoint("recovered:" + filepath.Base(entry.Path))
		}
		if err := os.Remove(journalPath); err != nil {
			return err
		}
	}
	for _, entry := range j.Files {
		for _, suffix := range []string{".restore", ".prerestore", ".recover"} {
			if err := os.Remove(entry.Path + suffix); err != nil && !os.IsNotExist(err) {
				return err
			}
		}
	}
	if j.Committed {
		if err := os.Remove(journalPath); err != nil {
			return err
		}
	}
	return nil
}
