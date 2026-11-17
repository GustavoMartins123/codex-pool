package main

import (
	"strings"

	"codex-pool-proxy/internal/logredact"
)

func safeText(b []byte) string {
	s := string(b)
	s = strings.ReplaceAll(s, "\n", "\\n")
	s = strings.ReplaceAll(s, "\r", "\\r")
	return logredact.Redact(s)
}
