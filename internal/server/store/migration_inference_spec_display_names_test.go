package store

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// Migration 197 replaces inference_specs.display_profile with the two
// display name columns. A failure part way leaves the old shape and no
// marker; a successful run keeps every row, every index and the other
// columns' values; a rerun changes nothing.
func TestInferenceSpecDisplayNamesMigration(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:inference-spec-display-names?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}, &InferenceSpecRow{}); err != nil {
		t.Fatal(err)
	}
	// The shape before this migration.
	for _, stmt := range []string{
		"ALTER TABLE inference_specs DROP COLUMN display_name_zh_cn",
		"ALTER TABLE inference_specs DROP COLUMN display_name_en",
		"ALTER TABLE inference_specs ADD COLUMN display_profile varchar(16)",
		`INSERT INTO inference_specs (model_id, source, name, display_profile, engine, image, weight_sources, gpus)
		 VALUES (1, 'upstream', 'bf16-vllm-nvidia-general', 'general', 'vllm', 'img:1', '[]', '{"vendor":"nvidia","cc":">=8.0","display_target":"nvidia"}')`,
	} {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	indexesBefore, err := indexDefinitions(db, "inference_specs")
	if err != nil {
		t.Fatal(err)
	}

	m := migrationByName(t, "inference_spec_display_names")
	failing := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return errors.New("injected failure")
	}}
	if err := migrateDB(db, []migration{failing}); err == nil {
		t.Fatal("injected failure was not reported")
	}
	if db.Migrator().HasColumn(&InferenceSpecRow{}, "DisplayNameZHCN") || !db.Migrator().HasColumn(&inferenceSpecDisplayProfileColumn{}, "DisplayProfile") {
		t.Fatal("a failed migration changed the schema")
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", m.Version).Count(&markers).Error; err != nil || markers != 0 {
		t.Fatalf("marker count = %d, err = %v", markers, err)
	}

	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("migration err = %v", err)
	}
	for _, column := range []string{"DisplayNameZHCN", "DisplayNameEN"} {
		if !db.Migrator().HasColumn(&InferenceSpecRow{}, column) {
			t.Fatalf("missing column %s", column)
		}
	}
	if db.Migrator().HasColumn(&inferenceSpecDisplayProfileColumn{}, "DisplayProfile") {
		t.Fatal("display_profile survived")
	}
	indexesAfter, err := indexDefinitions(db, "inference_specs")
	if err != nil {
		t.Fatal(err)
	}
	if len(indexesAfter) != len(indexesBefore) {
		t.Fatalf("indexes %d -> %d", len(indexesBefore), len(indexesAfter))
	}
	var row InferenceSpecRow
	if err := db.First(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.Name != "bf16-vllm-nvidia-general" || row.Image != "img:1" || row.DisplayNameZHCN != "" || row.DisplayNameEN != "" {
		t.Fatalf("row after migration = %+v", row)
	}
	var marker SchemaMigration
	if err := db.First(&marker, m.Version).Error; err != nil || marker.Name != m.Name {
		t.Fatalf("marker = %+v, err = %v", marker, err)
	}

	if err := db.Delete(&SchemaMigration{}, m.Version).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("rerun err = %v", err)
	}
}
