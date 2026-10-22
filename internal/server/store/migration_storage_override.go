package store

import "gorm.io/gorm"

func migrateStorageNodeOverrideScope(db *gorm.DB) error {
	if db.Migrator().HasColumn(&StorageNode{}, "OverrideOnly") {
		return nil
	}
	return db.Migrator().AddColumn(&StorageNode{}, "OverrideOnly")
}
