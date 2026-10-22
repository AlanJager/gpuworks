package store

import "gorm.io/gorm"

// migrateImageCatalogSimplify prepares the catalog for the system image sync
// that now runs on every start.
//
// It indexes the two columns that name an image by ref: removing a system
// image from the catalog counts its references (ImageReferenceCounts), and
// without these indexes every count scans all deployments and instances.
//
// It also drops the image_catalog_version setting. The catalog version used to
// gate the seed import and refuse downgrades; the sync is idempotent and no
// longer compares versions, so nothing reads the row.
func migrateImageCatalogSimplify(db *gorm.DB) error {
	for _, step := range []struct{ table, statement string }{
		{"deployments", "CREATE INDEX IF NOT EXISTS idx_deployments_image ON deployments (image)"},
		{"instances", "CREATE INDEX IF NOT EXISTS idx_instances_image ON instances (image)"},
		{"settings", "DELETE FROM settings WHERE key = 'image_catalog_version'"},
	} {
		if !db.Migrator().HasTable(step.table) {
			continue
		}
		if err := db.Exec(step.statement).Error; err != nil {
			return err
		}
	}
	return nil
}
