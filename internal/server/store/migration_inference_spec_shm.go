package store

import "gorm.io/gorm"

// addInferenceSpecShmBytes adds the /dev/shm size a template spec states
// (InferenceSpecRow.ShmBytes). Existing specs state none, which launch
// resolution reads as the engine default.
func addInferenceSpecShmBytes(db *gorm.DB) error {
	if !db.Migrator().HasTable(&InferenceSpecRow{}) || db.Migrator().HasColumn(&InferenceSpecRow{}, "ShmBytes") {
		return nil
	}
	if err := db.Migrator().AddColumn(&InferenceSpecRow{}, "ShmBytes"); err != nil {
		return err
	}
	return db.Exec("UPDATE inference_specs SET shm_bytes = 0 WHERE shm_bytes IS NULL").Error
}
