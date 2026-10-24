//go:build !windows

package runner

import "context"

func checkCodexSandbox(context.Context, string, string, string) error { return nil }
