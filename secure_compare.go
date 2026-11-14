package main

import (
	"crypto/subtle"
)

// secureSecretEquals compares two secret strings without leaking match
// length or prefix information. Both sides are hashed to a fixed length
// first so the constant-time comparison never reveals how much of the
// raw secret agreed.
func secureSecretEquals(a, b string) bool {
	ah := hashToken(a)
	bh := hashToken(b)
	return subtle.ConstantTimeCompare(ah[:], bh[:]) == 1
}
