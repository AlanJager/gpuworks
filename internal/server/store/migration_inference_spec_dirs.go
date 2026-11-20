package store

import "gorm.io/gorm"

// addInferenceSpecDirs adds the column holding the writable directories a
// template spec declares (InferenceSpecRow.Dirs). Existing specs declare none,
// which the empty value means; rows already stored get it instead of NULL,
// which a string field cannot read.
func addInferenceSpecDirs(db *gorm.DB) error {
	if !db.Migrator().HasTable(&InferenceSpecRow{}) || db.Migrator().HasColumn(&InferenceSpecRow{}, "Dirs") {
		return nil
	}
	if err := db.Migrator().AddColumn(&InferenceSpecRow{}, "Dirs"); err != nil {
		return err
	}
	return db.Exec("UPDATE inference_specs SET dirs = '' WHERE dirs IS NULL").Error
}
