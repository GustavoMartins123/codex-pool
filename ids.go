package main

import (
	"crypto/rand"
	"encoding/hex"
)

func randomID() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b[:])
}
