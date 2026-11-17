//go:build !windows

// Package pathmatch compares filesystem paths with the case semantics of the
// host platform.
package pathmatch

import (
	"path/filepath"
	"strings"
)

// Equal compares filesystem paths with POSIX case-sensitive semantics. Both
// sides are cleaned first: fsnotify composes event names by concatenating the
// watched directory with the reported name, so a relative watch of "." yields
// "./config.toml", which must still match a configured "config.toml".
func Equal(a, b string) bool {
	return filepath.Clean(a) == filepath.Clean(b)
}

// Within reports whether target is dir itself or nested inside it.
func Within(dir, target string) bool {
	dir = filepath.Clean(dir)
	target = filepath.Clean(target)
	if target == dir {
		return true
	}
	return strings.HasPrefix(target, dir+string(filepath.Separator))
}
