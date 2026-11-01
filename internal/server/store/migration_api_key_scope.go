package store

import "gorm.io/gorm"

// addAPIKeyScope lets an API key be limited to chosen deployments. Every
// existing key keeps reaching all of them.
func addAPIKeyScope(db *gorm.DB) error {
	if !db.Migrator().HasTable(&APIKey{}) {
		return nil
	}
	if !db.Migrator().HasColumn(&APIKey{}, "Restricted") {
		if err := db.Migrator().AddColumn(&APIKey{}, "Restricted"); err != nil {
			return err
		}
	}
	return db.AutoMigrate(&APIKeyDeployment{})
}
