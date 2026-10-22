package store

import "time"

// Setting is a key-value store for platform config (network/accel presets, etc.).
type Setting struct {
	Key       string `gorm:"primaryKey;size:64"`
	Value     string `gorm:"type:text"` // JSON; the owner type is fixed per key (see the SettingKey constants)
	UpdatedAt time.Time
}
