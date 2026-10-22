package store

import "gorm.io/gorm"

// addNodeAcceleratorChoice adds the operator's choice of which vendor's
// cards a node manages and the vendors its host has
// (Node.AcceleratorPreference/AcceleratorVendors). Both stay empty until
// someone chooses and the node's next report lists its bus.
func addNodeAcceleratorChoice(db *gorm.DB) error {
	if !db.Migrator().HasTable(&Node{}) {
		return nil
	}
	for _, field := range []string{"AcceleratorPreference", "AcceleratorVendors"} {
		if db.Migrator().HasColumn(&Node{}, field) {
			continue
		}
		if err := db.Migrator().AddColumn(&Node{}, field); err != nil {
			return err
		}
	}
	return nil
}
