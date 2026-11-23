// Package ids generates short random identifiers.
package ids

import (
	"crypto/rand"
	"encoding/hex"
)

func Random() string {
	var b [6]byte
	if _, err := rand.Read(b[:]); err != nil {
		return "unknown"
	}
	return hex.EncodeToString(b[:])
}
