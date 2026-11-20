package store

import "gorm.io/gorm"

// migrateCatalogGCBarrier adds the single row a catalog package garbage
// collection must rewrite before it trusts a reference list. The row is a
// committed write to wait for, not a task ledger or a resource manifest.
func migrateCatalogGCBarrier(db *gorm.DB) error {
	if err := db.Exec(`CREATE TABLE catalog_gc_barriers (
		id INTEGER PRIMARY KEY CHECK (id = 1),
		sequence INTEGER NOT NULL CHECK (typeof(sequence) = 'integer' AND sequence >= 0)
	)`).Error; err != nil {
		return err
	}
	return db.Exec("INSERT INTO catalog_gc_barriers (id, sequence) VALUES (1, 0)").Error
}
