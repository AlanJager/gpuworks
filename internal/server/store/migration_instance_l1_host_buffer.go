package store

import "gorm.io/gorm"

// instanceL1HostBufferColumn is instances.l1_host_buffer_held as migration
// 192 adds it.
type instanceL1HostBufferColumn struct {
	L1HostBufferHeld bool `gorm:"not null;default:false"`
}

func (instanceL1HostBufferColumn) TableName() string { return "instances" }

// addInstanceL1HostBuffer adds whether an instance's engine already holds the
// pinned host memory an L1 sleep uses (Instance.L1HostBufferHeld). Existing
// rows are not known to: the next report of their agent says, and until then
// an L1 sleep of them is checked against the host's memory as before.
func addInstanceL1HostBuffer(db *gorm.DB) error {
	if !db.Migrator().HasTable(&Instance{}) || db.Migrator().HasColumn(&instanceL1HostBufferColumn{}, "L1HostBufferHeld") {
		return nil
	}
	return db.Migrator().AddColumn(&instanceL1HostBufferColumn{}, "L1HostBufferHeld")
}
