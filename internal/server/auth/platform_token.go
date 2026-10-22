package auth

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"strings"
)

// Platform tokens authenticate the management API and MCP as a user, the
// way a login session does; inference API keys ("gw-...") authenticate /v1
// only. The prefix tells the two apart at a glance, in a header or a leak.
const (
	PlatformTokenPrefix    = "gwp_"
	PlatformTokenPrefixLen = 12 // what the UI shows: "gwp_" and 8 characters
)

// NewPlatformToken returns a fresh 256-bit token and its displayed prefix.
func NewPlatformToken() (full, prefix string) {
	b := make([]byte, 32)
	_, _ = rand.Read(b)
	full = PlatformTokenPrefix + base64.RawURLEncoding.EncodeToString(b)
	return full, full[:PlatformTokenPrefixLen]
}

// IsPlatformToken reports whether a bearer value is a platform token.
func IsPlatformToken(token string) bool { return strings.HasPrefix(token, PlatformTokenPrefix) }

// HashPlatformToken is what the store keeps of a token. A 256-bit random
// secret needs no slow hash: nothing about it can be guessed offline.
func HashPlatformToken(token string) string {
	sum := sha256.Sum256([]byte(token))
	return hex.EncodeToString(sum[:])
}
