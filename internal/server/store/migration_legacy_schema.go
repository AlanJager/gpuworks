package store

import "gorm.io/gorm"

// autoMigratePreservingGPUIndexes retains historical indexes that are no
// longer declared on GPU. SQLite's INTEGER-to-NUMERIC boolean conversion
// rebuilds the table, and GORM recreates only indexes in the current model.
// The old device-index identity remains enforced until its explicit cutover.
func autoMigratePreservingGPUIndexes(db *gorm.DB, models ...any) error {
	indexes, err := indexDefinitions(db, "gpus")
	if err != nil {
		return err
	}
	if err := db.AutoMigrate(models...); err != nil {
		return err
	}
	for name, definition := range indexes {
		if !db.Migrator().HasIndex("gpus", name) {
			if err := db.Exec(definition).Error; err != nil {
				return err
			}
		}
	}
	return nil
}

func migrateInferenceSpecSourceIdentity(db *gorm.DB) error {
	if db.Migrator().HasIndex(&InferenceSpecRow{}, "idx_inference_spec_identity") {
		if err := db.Migrator().DropIndex(&InferenceSpecRow{}, "idx_inference_spec_identity"); err != nil {
			return err
		}
	}
	if err := backfillInferenceSpecOwnership(db); err != nil {
		return err
	}
	if err := db.AutoMigrate(&InferenceSpecRow{}, &Deployment{}); err != nil {
		return err
	}
	return db.Exec("CREATE UNIQUE INDEX IF NOT EXISTS idx_inference_spec_identity ON inference_specs(model_id, source, name)").Error
}

// Earlier catalog migrations also use the current model, whose ownership is
// required. Repair historical nullable values before that model rebuilds the
// table; waiting for v72 would prevent those databases from reaching v72.
func migrateInferenceCatalogContracts(db *gorm.DB) error {
	if db.Migrator().HasTable(&InferenceSpecRow{}) {
		if err := backfillInferenceSpecOwnership(db); err != nil {
			return err
		}
	}
	return db.AutoMigrate(&InferenceModel{}, &InferenceSpecRow{})
}

func backfillInferenceSpecOwnership(db *gorm.DB) error {
	// Add ownership without the current default first: a default of upstream
	// would hide which historical specs belong to a user-owned model. Repair
	// nullable historical values before AutoMigrate enforces NOT NULL.
	if !db.Migrator().HasColumn(&InferenceSpecRow{}, "Source") {
		if err := db.Exec(`ALTER TABLE inference_specs ADD COLUMN source TEXT`).Error; err != nil {
			return err
		}
	}
	if !db.Migrator().HasColumn(&InferenceSpecRow{}, "PackageDigest") {
		if err := db.Migrator().AddColumn(&InferenceSpecRow{}, "PackageDigest"); err != nil {
			return err
		}
	}
	return db.Exec(`UPDATE inference_specs
		SET source = COALESCE(NULLIF(trim(source), ''),
			(SELECT NULLIF(trim(source), '') FROM inference_models WHERE inference_models.id = inference_specs.model_id), 'upstream'),
		package_digest = COALESCE(NULLIF(trim(package_digest), ''),
			(SELECT package_digest FROM inference_models WHERE inference_models.id = inference_specs.model_id), '')`).Error
}

// migrateLegacySchemaRepairs reconciles databases that already recorded the
// buggy historical migrations. A matching package digest proves the user
// package owns a spec; mixed catalogs with another digest or a conflicting
// user spec retain their explicit ownership.
func migrateLegacySchemaRepairs(db *gorm.DB) error {
	if err := db.Exec("DROP TABLE IF EXISTS legacy_jobs").Error; err != nil {
		return err
	}
	if !db.Migrator().HasTable(&InferenceModel{}) || !db.Migrator().HasTable(&InferenceSpecRow{}) {
		return nil
	}
	repairable := db.Table("inference_specs AS spec").Select("spec.id").
		Joins("JOIN inference_models model ON model.id = spec.model_id").
		Where("spec.source = 'upstream' AND model.source = 'user'").
		Where("model.package_digest IS NOT NULL AND trim(model.package_digest) <> '' AND model.package_digest = spec.package_digest").
		Where(`NOT EXISTS (SELECT 1 FROM inference_specs sibling
			WHERE sibling.model_id = spec.model_id AND sibling.name = spec.name AND sibling.source = 'user')`)
	if db.Migrator().HasColumn(&Deployment{}, "TemplateSpecSource") {
		// Preserve each deployment's reference to the same spec when its
		// ownership key changes. Its launch snapshot remains historical.
		identities := db.Table("inference_specs").Select("model_id, name").Where("id IN (?)", repairable)
		if err := db.Model(&Deployment{}).
			Where("template_spec_source = 'upstream' AND (template_id, template_spec_name) IN (?)", identities).
			UpdateColumn("template_spec_source", "user").Error; err != nil {
			return err
		}
	}
	return db.Table("inference_specs").Where("id IN (?)", repairable).UpdateColumn("source", "user").Error
}
