package store

import "gorm.io/gorm"

// addUserIdentities creates user_identities and adds users.disabled_at
// (ADR 0085). The baseline migration creates both from the current entities on
// a new database, so there they already exist and only the marker is written.
// No row is backfilled: every existing user is enabled and no user has an
// external identity yet.
func addUserIdentities(db *gorm.DB) error {
	if !db.Migrator().HasTable(&User{}) {
		return nil
	}
	if !db.Migrator().HasColumn(&User{}, "DisabledAt") {
		if err := db.Migrator().AddColumn(&User{}, "DisabledAt"); err != nil {
			return err
		}
	}
	return db.AutoMigrate(&UserIdentity{})
}
