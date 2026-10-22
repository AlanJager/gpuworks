package auth

import (
	"crypto/rand"
	"encoding/hex"
	"strings"
)

// APIKeyPrefixLen is the clear-text prefix length stored for O(1) lookup.
const APIKeyPrefixLen = 12

// NewAPIKey returns (fullKey, prefix). The full key is shown to the user once;
// only its prefix (clear) and argon2id hash are stored.
func NewAPIKey() (full, prefix string) {
	b := make([]byte, 24)
	_, _ = rand.Read(b)
	secret := hex.EncodeToString(b)
	full = "gw-" + secret
	prefix = full[:APIKeyPrefixLen]
	return full, prefix
}

// PrefixOf returns the storage prefix for a presented key.
func PrefixOf(key string) string {
	if len(key) < APIKeyPrefixLen {
		return key
	}
	return key[:APIKeyPrefixLen]
}

// BearerToken extracts a token from an Authorization header value.
func BearerToken(header string) string {
	if header == "" {
		return ""
	}
	if strings.HasPrefix(header, "Bearer ") {
		return strings.TrimSpace(header[len("Bearer "):])
	}
	return strings.TrimSpace(header)
}
