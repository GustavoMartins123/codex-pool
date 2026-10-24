//go:build windows

package atomicfile

import (
	"golang.org/x/sys/windows"
	"os"
)

// Publish commits an already-synced file with the write-through rename API.
// Cross-volume moves and unsupported operations are errors, never copies.
func Publish(source, destination string) error {
	from, err := extendedPath(source)
	if err != nil {
		return err
	}
	to, err := extendedPath(destination)
	if err != nil {
		return err
	}
	from16, err := windows.UTF16PtrFromString(from)
	if err != nil {
		return err
	}
	to16, err := windows.UTF16PtrFromString(to)
	if err != nil {
		return err
	}
	if err := windows.MoveFileEx(from16, to16, windows.MOVEFILE_REPLACE_EXISTING|windows.MOVEFILE_WRITE_THROUGH); err != nil {
		return &os.LinkError{Op: "publish", Old: source, New: destination, Err: err}
	}
	return nil
}
