package atomicfile

import (
	"os"
	"path/filepath"
	"testing"
)

func TestPublishCommitsSyncedFileAndLeavesDestinationOnFailure(t *testing.T) {
	dir := t.TempDir()
	source := filepath.Join(dir, "temporary")
	destination := filepath.Join(dir, "published")
	if err := os.WriteFile(destination, []byte("previous"), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := os.OpenFile(source, os.O_CREATE|os.O_WRONLY|os.O_EXCL, 0600)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.Write([]byte("committed")); err != nil {
		t.Fatal(err)
	}
	if err := file.Sync(); err != nil {
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
	if err := Publish(source, destination); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(destination)
	if err != nil || string(raw) != "committed" {
		t.Fatal("publication failed")
	}
	if err := Publish(source, destination); err == nil {
		t.Fatal("missing source accepted")
	}
	raw, err = os.ReadFile(destination)
	if err != nil || string(raw) != "committed" {
		t.Fatal("failed publication damaged the canonical file")
	}
}
