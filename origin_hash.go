package main

import (
	"net/http"
	"strings"

	"codex-pool-proxy/internal/clientip"
)

func poolHashSalt(legacySalt string) string {
	legacySalt = strings.TrimSpace(legacySalt)
	if legacySalt != "" {
		return legacySalt
	}
	return "codex-pool"
}

func hashRequestOrigin(r *http.Request, salt string) string {
	if r == nil {
		return ""
	}
	ip := clientip.Get(r)
	if ip == "" {
		return ""
	}
	return "ip_" + hashUserIP(ip, salt)
}
