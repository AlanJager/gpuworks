package store

import "time"

// Profile is the retired calibration schema, retained for historical migrations
// and cleanup of legacy model records. Runtime reports no longer write profiles,
// and estimates never read them.
type Profile struct {
	ID          uint   `gorm:"primaryKey"`
	Fingerprint string `gorm:"uniqueIndex;size:64"`
	ModelID     uint   `gorm:"index"`
	NodeID      uint   `gorm:"index"`
	Engine      string `gorm:"size:32;index"`
	Vendor      string `gorm:"size:32;index"`
	ArchID      string `gorm:"size:64"`
	ArgsHash    string `gorm:"size:64;index"`
	Measured    string `gorm:"type:text"` // JSON enginelog.Measured, as Instance.Measured
	CreatedAt   time.Time
	UpdatedAt   time.Time
}
