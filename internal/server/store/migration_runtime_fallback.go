package store

import "gorm.io/gorm"

// addNodeRuntimeFallback adds the runtime a node's auto-detection passed
// over and why (Node.RuntimeFallbackFrom/Reason). Both stay empty until each
// node's next report says otherwise.
func addNodeRuntimeFallback(db *gorm.DB) error {
	if !db.Migrator().HasTable(&Node{}) {
		return nil
	}
	for _, field := range []string{"RuntimeFallbackFrom", "RuntimeFallbackReason"} {
		if db.Migrator().HasColumn(&Node{}, field) {
			continue
		}
		if err := db.Migrator().AddColumn(&Node{}, field); err != nil {
			return err
		}
	}
	return nil
}
