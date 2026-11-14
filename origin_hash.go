package main

import (
	"net/http"
	"strings"
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
	ip := getClientIP(r)
	if ip == "" {
		return ""
	}
	return "ip_" + hashUserIP(ip, salt)
}
