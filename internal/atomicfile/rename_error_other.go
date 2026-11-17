//go:build !windows

package atomicfile

// IsTransientRenameError reports whether a failed rename is worth retrying.
// POSIX rename(2) over an open descriptor always succeeds; its failures are
// deterministic (permissions, missing paths) and retrying would only delay the
// real error.
func IsTransientRenameError(err error) bool {
	return false
}
