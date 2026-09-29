//go:build windows

package atomicfile

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestReplaceNormalizesMixedSeparators(t *testing.T) {
	dir := t.TempDir()
	destination := dir + "/target.json"
	source := filepath.Join(dir, "new.tmp")
	if err := os.WriteFile(destination, []byte("old"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(source, []byte("new"), 0600); err != nil {
		t.Fatal(err)
	}
	if err := Replace(strings.ReplaceAll(source, `\`, "/"), destination); err != nil {
		t.Fatal(err)
	}
	raw, err := os.ReadFile(destination)
	if err != nil || string(raw) != "new" {
		t.Fatalf("replacement: %q, %v", raw, err)
	}
}
