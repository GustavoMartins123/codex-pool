//go:build !windows

package runner

import "os"

func privateFile(file *os.File) error { return file.Chmod(0600) }
