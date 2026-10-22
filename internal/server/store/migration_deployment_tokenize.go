package store

import "gorm.io/gorm"

// addDeploymentTokenizeDisabled adds the deployment's tokenize switch. The
// column is the inverse of the switch, so every existing deployment keeps the
// gateway's tokenize routes open.
func addDeploymentTokenizeDisabled(db *gorm.DB) error {
	if !db.Migrator().HasTable(&Deployment{}) || db.Migrator().HasColumn(&Deployment{}, "TokenizeDisabled") {
		return nil
	}
	return db.Migrator().AddColumn(&Deployment{}, "TokenizeDisabled")
}
