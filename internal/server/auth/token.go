// Package auth holds shared credential primitives used by management JWTs,
// API keys and per-node enrollment tokens.
package auth

import (
	"crypto/rand"
	"encoding/hex"
)

// NewToken returns a cryptographically random token (n bytes, hex-encoded).
func NewToken(n int) string {
	if n <= 0 {
		n = 24
	}
	b := make([]byte, n)
	if _, err := rand.Read(b); err != nil {
		// crypto/rand failure is fatal-worthy; return empty so callers can guard.
		return ""
	}
	return hex.EncodeToString(b)
}

// NewUUID returns a random 16-byte hex identifier (node UUIDs, etc.).
func NewUUID() string {
	return NewToken(16)
}
