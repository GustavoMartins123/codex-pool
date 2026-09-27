package main

import (
	"fmt"
	"io/fs"
	"log"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

// verifySensitiveFilePermissions audits the pool directory on POSIX systems:
// credential files must not be readable beyond their owner. Returns the list
// of problems found. On Windows the permission model does not map to POSIX
// modes, so the check is skipped.
//
// A permissive pool *directory* is remediated in place (chmod 0700) because a
// bind-mounted host directory routinely ships as 0755 and the fix is safe to
// apply; a directory that cannot be tightened is still reported. Permissive
// credential *files* are never auto-fixed: the caller fails startup so the
// operator decides whether the file or its ownership needs to change.
func verifySensitiveFilePermissions(poolDir string) []string {
	if runtime.GOOS == "windows" || strings.TrimSpace(poolDir) == "" {
		return nil
	}
	var problems []string
	if info, err := os.Stat(poolDir); err == nil && info.IsDir() && info.Mode().Perm()&0o077 != 0 {
		if chmodErr := os.Chmod(poolDir, 0o700); chmodErr != nil {
			problems = append(problems, fmt.Sprintf("%s (directory, chmod 0700 failed: %v)", poolDir, chmodErr))
		} else {
			log.Printf("security: tightened pool directory permissions to 0700: %s", poolDir)
		}
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
	return problems
}
