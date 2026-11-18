package store

import "gorm.io/gorm"

// addUserLocalAdmin adds User.LocalAdmin, the break-glass floor of the
// plugin host (ADR 0071, decision 11), and gives it to every administrator
// the database already has. The baseline migration creates the column from
// the current entity on a new database, so only the backfill runs there; it
// runs once either way, under this migration's marker.
func addUserLocalAdmin(db *gorm.DB) error {
	if !db.Migrator().HasTable(&User{}) {
		return nil
	}
	if !db.Migrator().HasColumn(&User{}, "LocalAdmin") {
		if err := db.Migrator().AddColumn(&User{}, "LocalAdmin"); err != nil {
			return err
		}
	}
	return db.Exec(`UPDATE users SET local_admin = 1 WHERE role = 'admin'`).Error
}
