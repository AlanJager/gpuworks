package store

import "gorm.io/gorm"

// addAPIKeyRevokedAt turns revoking a key from deleting its row into marking
// it, so the usage a key had keeps its name. Every existing key is live.
func addAPIKeyRevokedAt(db *gorm.DB) error {
	if !db.Migrator().HasTable(&APIKey{}) || db.Migrator().HasColumn(&APIKey{}, "RevokedAt") {
		return nil
	}
	return db.Migrator().AddColumn(&APIKey{}, "RevokedAt")
}
