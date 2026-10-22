package store

import "gorm.io/gorm"

// dropUnusedImageAndStorageColumns is migration 113. Nothing wrote or read
// image_versions.adapted_models, and storages.config only ever held the seed's
// empty object. A missing table or column is left alone.
func dropUnusedImageAndStorageColumns(db *gorm.DB) error {
	if err := dropColumns(db, "image_versions", "adapted_models"); err != nil {
		return err
	}
	return dropColumns(db, "storages", "config")
}

// dropUnusedImageAdaptationColumns is migration 115. No write path stored a
// value in the image catalog's adaptation columns and nothing read them:
// vendors are derived from the framework (accelerator.FrameworkVendor), and
// free-form GPU labels were never a selection input. Inputs that still carry
// those keys import with the keys ignored. A missing table or column is left
// alone.
func dropUnusedImageAdaptationColumns(db *gorm.DB) error {
	if err := dropColumns(db, "image_series", "vendors"); err != nil {
		return err
	}
	return dropColumns(db, "image_versions", "vendors", "supported_gp_us", "unsupported_gp_us")
}
