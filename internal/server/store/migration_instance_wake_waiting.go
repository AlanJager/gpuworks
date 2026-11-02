package store

import "gorm.io/gorm"

// addInstanceWakeWaiting adds when a wake that did not fit started waiting
// for room (nil = none waits). Existing rows wait for nothing.
func addInstanceWakeWaiting(db *gorm.DB) error {
	if !db.Migrator().HasTable(&Instance{}) {
		return nil
	}
	if !db.Migrator().HasColumn(&Instance{}, "WakeWaitingSince") {
		if err := db.Migrator().AddColumn(&Instance{}, "WakeWaitingSince"); err != nil {
			return err
		}
	}
	if !db.Migrator().HasIndex(&Instance{}, "WakeWaitingSince") {
		return db.Migrator().CreateIndex(&Instance{}, "WakeWaitingSince")
	}
	return nil
}
