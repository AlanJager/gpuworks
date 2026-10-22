package store

import "gorm.io/gorm"

// migrateInferenceSpecImageSources adds inference_specs.image_sources: the
// further registry references a catalog spec lists for its runtime image
// (ADR 0038). Every existing spec lists none, which reads as the empty string.
func migrateInferenceSpecImageSources(db *gorm.DB) error {
	if !db.Migrator().HasTable(&InferenceSpecRow{}) || db.Migrator().HasColumn(&InferenceSpecRow{}, "ImageSources") {
		return nil
	}
	if err := db.Migrator().AddColumn(&InferenceSpecRow{}, "ImageSources"); err != nil {
		return err
	}
	return db.Exec("UPDATE inference_specs SET image_sources = '' WHERE image_sources IS NULL").Error
}
