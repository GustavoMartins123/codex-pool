//go:build !windows

package credstore

import "os"

func openCredentialFile(path string) (*os.File, error) {
	return os.Open(path)
}
