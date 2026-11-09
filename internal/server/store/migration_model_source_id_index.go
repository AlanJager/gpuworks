package store

import "gorm.io/gorm"

// createModelSourceIDIndex indexes models by case-folded repository id alone.
// The deploy evaluation looks up the registered models serving a template's
// weight sources by repository id across every source; the identity index
// leads with lower(source), so without this that lookup reads every row.
func createModelSourceIDIndex(db *gorm.DB) error {
	if !db.Migrator().HasTable(&Model{}) {
		return nil
	}
	return db.Exec("CREATE INDEX IF NOT EXISTS idx_model_source_id_lower ON models(lower(source_id))").Error
}
