//go:build windows

package main

import (
	"os"
	"strings"
)

// pathsEqual compares filesystem paths with NTFS case-insensitive semantics:
// a CONFIG_PATH whose casing differs from the on-disk name must still match
// the (on-disk-cased) names that ReadDirectoryChangesW reports.
func pathsEqual(a, b string) bool {
	return strings.EqualFold(a, b)
}

// pathWithin reports whether target is dir itself or nested inside it.
func pathWithin(dir, target string) bool {
	if pathsEqual(dir, target) {
		return true
	}
	if len(target) <= len(dir) {
		return false
	}
	if !pathsEqual(dir, target[:len(dir)]) {
		return false
	}
	// fsnotify may report either separator on Windows.
	return target[len(dir)] == os.PathSeparator || target[len(dir)] == '/'
}
