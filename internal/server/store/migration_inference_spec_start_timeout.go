package store

import "gorm.io/gorm"

// addInferenceSpecStartTimeout adds the instance start timeout a template spec
// states (InferenceSpecRow.StartTimeoutSec). Existing specs state none, so
// they read as zero and keep following the cluster default.
func addInferenceSpecStartTimeout(db *gorm.DB) error {
	if !db.Migrator().HasTable(&InferenceSpecRow{}) || db.Migrator().HasColumn(&InferenceSpecRow{}, "StartTimeoutSec") {
		return nil
	}
	if err := db.Migrator().AddColumn(&InferenceSpecRow{}, "StartTimeoutSec"); err != nil {
		return err
	}
	return db.Exec("UPDATE inference_specs SET start_timeout_sec = 0 WHERE start_timeout_sec IS NULL").Error
}
