package store

import "gorm.io/gorm"

// addInstanceOpenAPIPaths stores the path list the agent read from the
// engine's OpenAPI document. Existing rows stay empty, which means unknown.
func addInstanceOpenAPIPaths(db *gorm.DB) error {
	if !db.Migrator().HasTable(&Instance{}) || db.Migrator().HasColumn(&Instance{}, "OpenAPIPaths") {
		return nil
	}
	return db.Migrator().AddColumn(&Instance{}, "OpenAPIPaths")
}
