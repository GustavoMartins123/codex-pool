//go:build windows

package credstore

import (
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/windows"
)

// Keep the opened file's snapshot readable while an atomic writer replaces
// its directory entry. Sharing DELETE avoids blocking the rename or a reader
// opening the replacement while the old snapshot is pending deletion.
func openCredentialFile(path string) (*os.File, error) {
	absolute, err := filepath.Abs(filepath.Clean(path))
	if err != nil {
		return nil, err
	}
	absolute = strings.ReplaceAll(absolute, "/", `\`)
	if !strings.HasPrefix(absolute, `\\?\`) {
		if strings.HasPrefix(absolute, `\\`) {
			absolute = `\\?\UNC\` + strings.TrimPrefix(absolute, `\\`)
		} else {
			absolute = `\\?\` + absolute
		}
	}
	name, err := windows.UTF16PtrFromString(absolute)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	handle, err := windows.CreateFile(name, windows.GENERIC_READ,
		windows.FILE_SHARE_READ|windows.FILE_SHARE_WRITE|windows.FILE_SHARE_DELETE,
		nil, windows.OPEN_EXISTING, windows.FILE_ATTRIBUTE_NORMAL, 0)
	if err != nil {
		return nil, &os.PathError{Op: "open", Path: path, Err: err}
	}
	return os.NewFile(uintptr(handle), path), nil
}
