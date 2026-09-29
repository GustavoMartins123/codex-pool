//go:build windows

package main

import (
	"errors"
	"syscall"

	"golang.org/x/sys/windows"
)

// isTransientRenameError reports whether a failed rename is worth retrying.
// On Windows, MoveFileEx cannot replace a destination that any handle holds
// without FILE_SHARE_DELETE — antivirus scans, backup agents, search
// indexers, editors, and even Go's own os.Open (which never requests
// FILE_SHARE_DELETE). Those holders release the file after milliseconds, so
// a bounded retry converts the failure into a short delay instead of a lost
// credential update.
func isTransientRenameError(err error) bool {
	var errno syscall.Errno
	if !errors.As(err, &errno) {
		return false
	}
	return errno == windows.ERROR_ACCESS_DENIED || errno == windows.ERROR_SHARING_VIOLATION
}
