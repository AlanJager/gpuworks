package store

import "time"

// User is a platform account. A single admin is bootstrapped at startup.
type User struct {
	ID           uint   `gorm:"primaryKey"`
	Username     string `gorm:"uniqueIndex;size:64"`
	PasswordHash string `gorm:"size:256"` // argon2id
	Role         string `gorm:"size:16"`  // admin | user
	CreatedAt    time.Time
	UpdatedAt    time.Time
}

// APIKey authenticates data-plane (/v1) and API access. The prefix is stored in
// clear for O(1) lookup; the full key is verified against an argon2id hash.
type APIKey struct {
	ID        uint   `gorm:"primaryKey"`
	Name      string `gorm:"size:128"`
	Prefix    string `gorm:"uniqueIndex;size:16"` // first chars, clear
	Hash      string `gorm:"size:256"`            // argon2id of full key
	UserID    uint   `gorm:"index"`
	CreatedAt time.Time
	LastUsed  *time.Time
}

// ManagementSession stores only the hash of a browser or management credential.
// Deleting the row revokes every JWT or Cookie bound to that login.
type ManagementSession struct {
	TokenHash         string `gorm:"primaryKey;size:64"`
	UserID            uint   `gorm:"index"`
	CreatedAt         time.Time
	ExpiresAt         time.Time `gorm:"index"`
	AbsoluteExpiresAt time.Time `gorm:"index"`
}
