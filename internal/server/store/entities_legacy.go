package store

import "time"

// legacyJob preserves the historical migration shape only. Runtime code no
// longer exposes or reads this entity; migration 42 drops the old jobs table.
type legacyJob struct {
	ID        uint   `gorm:"primaryKey"`
	Kind      string `gorm:"size:32;index"`
	Name      string `gorm:"size:128"`
	State     string `gorm:"size:16;index"`
	Params    string `gorm:"type:text"` // JSON evalscope job params (type since removed)
	Spec      string `gorm:"type:text"` // JSON proto.JobSpec (type since removed)
	Result    string `gorm:"type:text"` // JSON job result the agent reported, stored undecoded
	NodeID    uint   `gorm:"index"`
	Message   string `gorm:"size:4096"`
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (legacyJob) TableName() string { return "jobs" }
