//go:build !windows

package main

// isTransientRenameError reports whether a failed rename is worth retrying.
// POSIX rename(2) over an open descriptor always succeeds; its failures are
// deterministic (permissions, missing paths) and retrying would only delay
// the real error.
func isTransientRenameError(err error) bool {
	return false
}
