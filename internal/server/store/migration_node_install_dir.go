package store

import "gorm.io/gorm"

// addNodeInstallDir adds where an SSH install put a node's agent
// (Node.InstallDir). Existing nodes stay empty, which recovery reads as the
// default directory it always used.
func addNodeInstallDir(db *gorm.DB) error {
	if !db.Migrator().HasTable(&Node{}) || db.Migrator().HasColumn(&Node{}, "InstallDir") {
		return nil
	}
	return db.Migrator().AddColumn(&Node{}, "InstallDir")
}
