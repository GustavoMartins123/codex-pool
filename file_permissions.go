package main

import (
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// verifySensitiveFilePermissions audits the pool directory on POSIX systems:
// credential files must not be readable beyond their owner. Returns the list
// of problems found (also logged by the caller). On Windows the permission
// model does not map to POSIX modes, so the check is skipped.
func verifySensitiveFilePermissions(poolDir string) []string {
	if runtime.GOOS == "windows" || strings.TrimSpace(poolDir) == "" {
		return nil
	}
	var problems []string
	if info, err := os.Stat(poolDir); err == nil && info.IsDir() && info.Mode().Perm()&0o077 != 0 {
		problems = append(problems, filepath.Join(poolDir, " (directory)"))
	}
	_ = filepath.Walk(poolDir, func(path string, info fs.FileInfo, err error) error {
		if err != nil || info.IsDir() || !strings.HasSuffix(info.Name(), ".json") {
			return nil
		}
		if info.Mode().Perm()&0o077 != 0 {
			problems = append(problems, path)
		}
		return nil
	})
	for _, path := range problems {
		log.Printf("security warning: sensitive file/directory is group/world accessible: %s (chmod 0600)", path)
	}
	return problems
}
