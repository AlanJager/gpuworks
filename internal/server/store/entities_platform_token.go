package store

import "time"

// Platform token scopes: a read token may only read (GET, and the read-only
// MCP endpoint); a write token may do whatever its user may.
const (
	PlatformTokenRead  = "read"
	PlatformTokenWrite = "write"
)

// PlatformToken authenticates the management API and MCP as its user. Only
// the SHA-256 of the token is kept; the clear prefix is for display.
type PlatformToken struct {
	ID         uint   `gorm:"primaryKey"`
	UserID     uint   `gorm:"index"`
	Name       string `gorm:"size:128"`
	Prefix     string `gorm:"size:16"`
	Hash       string `gorm:"uniqueIndex;size:64"`
	Scope      string `gorm:"size:8"` // PlatformTokenRead | PlatformTokenWrite
	ExpiresAt  *time.Time
	LastUsedAt *time.Time
	CreatedAt  time.Time
}
