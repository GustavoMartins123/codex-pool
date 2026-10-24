package runner

import (
	"errors"
	"os"
	"path/filepath"
)

func LockState(path string) (func(), error) {
	if err := os.MkdirAll(filepath.Dir(path), 0700); err != nil {
		return nil, err
	}
	file, err := os.OpenFile(path+".lock", os.O_CREATE|os.O_RDWR, 0600)
	if err != nil {
		return nil, err
	}
	if err := privateFile(file); err != nil {
		file.Close()
		return nil, err
	}
	if err := lockFile(file); err != nil {
		file.Close()
		return nil, errors.New("runner state is in use; stop the current runner before changing its credential")
	}
	return func() { file.Close() }, nil
}
