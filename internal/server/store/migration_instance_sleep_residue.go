package store

import "gorm.io/gorm"

// addInstanceSleepResidue adds the residue the agent measured after an
// instance's last sleep (0 = not measured, the ledger estimates). Existing
// rows have none.
func addInstanceSleepResidue(db *gorm.DB) error {
	if !db.Migrator().HasTable(&Instance{}) || db.Migrator().HasColumn(&Instance{}, "SleepResidueBytes") {
		return nil
	}
	return db.Migrator().AddColumn(&Instance{}, "SleepResidueBytes")
}
