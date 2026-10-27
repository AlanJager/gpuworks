package store

import "gorm.io/gorm"

// Existing transfer/archive sizes cannot backfill a runtime measurement.
func addImageNodeRuntimeSize(db *gorm.DB) error {
	if !db.Migrator().HasTable(&ImageNode{}) || db.Migrator().HasColumn(&ImageNode{}, "RuntimeSizeBytes") {
		return nil
	}
	return db.Migrator().AddColumn(&ImageNode{}, "RuntimeSizeBytes")
}
