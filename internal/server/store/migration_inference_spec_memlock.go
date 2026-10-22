package store

import "gorm.io/gorm"

// addInferenceSpecMemlockUnlimited adds the flag a template spec sets to start
// its container with unlimited locked memory
// (InferenceSpecRow.MemlockUnlimited). Existing specs state none, so they read
// as false.
func addInferenceSpecMemlockUnlimited(db *gorm.DB) error {
	if !db.Migrator().HasTable(&InferenceSpecRow{}) || db.Migrator().HasColumn(&InferenceSpecRow{}, "MemlockUnlimited") {
		return nil
	}
	if err := db.Migrator().AddColumn(&InferenceSpecRow{}, "MemlockUnlimited"); err != nil {
		return err
	}
	return db.Exec("UPDATE inference_specs SET memlock_unlimited = 0 WHERE memlock_unlimited IS NULL").Error
}
