//go:build windows

package credstore

import (
	"codex-pool-proxy/internal/atomicfile"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCredentialReaderRetainsSnapshotAcrossAtomicReplace(t *testing.T) {
	path := filepath.Join(t.TempDir(), "conta ã.json")
	if err := os.WriteFile(path, []byte("old snapshot"), 0600); err != nil {
		t.Fatal(err)
	}
	reader, err := openCredentialFile(path)
	if err != nil {
		t.Fatal(err)
	}
	defer reader.Close()
	if err := os.WriteFile(path+".tmp", []byte("new snapshot"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := atomicfile.Replace(path+".tmp", path); err != nil {
		t.Fatalf("credential reader blocked atomic replacement: %v", err)
	}
	old, err := io.ReadAll(reader)
	if err != nil || string(old) != "old snapshot" {
		t.Fatalf("opened snapshot changed: %q %v", old, err)
	}
	current, err := ReadFile(PlainStore{}, path)
	if err != nil || string(current) != "new snapshot" {
		t.Fatalf("replacement snapshot unreadable: %q %v", current, err)
	}
}

func TestCredentialReaderSupportsLongWindowsPath(t *testing.T) {
	dir := filepath.Join(t.TempDir(), strings.Repeat("a", 100), strings.Repeat("b", 100))
	if err := os.MkdirAll(dir, 0700); err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(dir, "cred.json")
	if err := os.WriteFile(path, []byte("complete"), 0600); err != nil {
		t.Fatal(err)
	}
	data, err := ReadFile(PlainStore{}, path)
	if err != nil || string(data) != "complete" {
		t.Fatalf("long path read failed: %q %v", data, err)
	}
}
