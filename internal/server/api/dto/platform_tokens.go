package dto

import "time"

// PlatformToken is a platform API token as listed: never the secret.
type PlatformToken struct {
	ID         uint       `json:"id"`
	Name       string     `json:"name"`
	Prefix     string     `json:"prefix"`
	Scope      string     `json:"scope"` // read | write
	CreatedAt  time.Time  `json:"created_at"`
	ExpiresAt  *time.Time `json:"expires_at"`
	LastUsedAt *time.Time `json:"last_used_at"`
}

// PlatformTokenRequest creates a token. Scope defaults to read; zero
// ExpiresInDays means the token does not expire.
type PlatformTokenRequest struct {
	Name          string `json:"name"`
	Scope         string `json:"scope,omitempty"`
	ExpiresInDays int    `json:"expires_in_days,omitempty"`
}

// PlatformTokenCreated carries the full token, shown this once.
type PlatformTokenCreated struct {
	Token string        `json:"token"`
	Item  PlatformToken `json:"item"`
}
