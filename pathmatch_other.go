//go:build !windows

package main

import (
	"os"
	"strings"
)

// pathsEqual compares filesystem paths with POSIX case-sensitive semantics.
func pathsEqual(a, b string) bool {
	return a == b
}

// pathWithin reports whether target is dir itself or nested inside it.
func pathWithin(dir, target string) bool {
	if target == dir {
		return true
	}
	return strings.HasPrefix(target, dir+string(os.PathSeparator))
}
