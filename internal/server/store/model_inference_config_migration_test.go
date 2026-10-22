package store

import (
	"encoding/json"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestModelInferenceConfigMigrationBackfillsLegacyFieldsAndIsIdempotent(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:model-inference-config-migration?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`CREATE TABLE models (id INTEGER PRIMARY KEY, capability_override TEXT, meta TEXT, operations TEXT, input_modalities TEXT)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO models(id, capability_override, meta) VALUES
		(1, 'embedding', ''),
		(2, '', '{"architectures":["Qwen2ForConditionalGeneration"],"vision_config":{"image_size":448}}')`).Error; err != nil {
		t.Fatal(err)
	}
	m := migrationByVersion(t, 80)
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	var rows []Model
	if err := db.Order("id").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	if len(rows) != 2 || rows[0].Operations != `[{"type":"openai.chat"}]` || rows[0].InputModalities != `["text"]` {
		t.Fatalf("legacy row = %+v", rows)
	}
	if rows[1].Operations != `[{"type":"openai.chat"}]` || rows[1].InputModalities != `["text","image"]` {
		t.Fatalf("legacy multimodal row = %+v", rows[1])
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("second migration: %v", err)
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", 80).Count(&markers).Error; err != nil || markers != 1 {
		t.Fatalf("marker count=%d err=%v", markers, err)
	}
	var raw string
	if err := db.Table("models").Select("operations").Where("id = 1").Scan(&raw).Error; err != nil {
		t.Fatal(err)
	}
	var decoded []map[string]string
	if err := json.Unmarshal([]byte(raw), &decoded); err != nil || decoded[0]["type"] != "openai.chat" {
		t.Fatalf("operations JSON = %q err=%v", raw, err)
	}
}
