//go:build !windows

package main

import (
	"path/filepath"
	"strings"
)

// pathsEqual compares filesystem paths with POSIX case-sensitive semantics.
// Both sides are cleaned first: fsnotify composes event names by
// concatenating the watched directory with the reported name, so a relative
// watch of "." yields "./config.toml", which must still match a configured
// "config.toml".
func pathsEqual(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
}

// pathWithin reports whether target is dir itself or nested inside it.
func pathWithin(dir, target string) bool {
	dir = filepath.Clean(dir)
	target = filepath.Clean(target)
	if target == dir {
		return true
	}
	return strings.HasPrefix(target, dir+string(filepath.Separator))
}
