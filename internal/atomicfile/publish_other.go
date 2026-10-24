//go:build !windows

package atomicfile

import (
	"os"
	"path/filepath"
)

func Publish(source, destination string) error {
	if filepath.Dir(source) != filepath.Dir(destination) {
		return &os.LinkError{Op: "publish", Old: source, New: destination, Err: os.ErrInvalid}
	}
	if err := os.Rename(source, destination); err != nil {
		return err
	}
	dir, err := os.Open(filepath.Dir(destination))
	if err != nil {
		return err
	}
	defer dir.Close()
	return dir.Sync()
}
