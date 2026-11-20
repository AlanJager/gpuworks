package store

import "gorm.io/gorm"

// addStorageNodeSharedFS adds the shared filesystem a node reports under a
// local storage's directory to its node state. Existing rows read as on a
// local disk until the node's next report.
func addStorageNodeSharedFS(db *gorm.DB) error {
	if !db.Migrator().HasTable(&StorageNodeState{}) {
		return nil
	}
	for _, col := range []string{"SharedFSType", "SharedFSSource"} {
		if !db.Migrator().HasColumn(&StorageNodeState{}, col) {
			if err := db.Migrator().AddColumn(&StorageNodeState{}, col); err != nil {
				return err
			}
		}
	}
	return nil
}
