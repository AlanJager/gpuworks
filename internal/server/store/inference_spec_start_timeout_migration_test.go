package store

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestInferenceSpecStartTimeoutMigration(t *testing.T) {
	open := func(name string) *gorm.DB {
		db, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"), &gorm.Config{})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("CREATE TABLE inference_specs (id INTEGER PRIMARY KEY, name TEXT)").Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("INSERT INTO inference_specs (id, name) VALUES (1, 'bf16')").Error; err != nil {
			t.Fatal(err)
		}
		return db
	}
	m := migrationByVersion(t, 230)
	if m.Name != "inference_spec_start_timeout" {
		t.Fatalf("migration 230 = %q", m.Name)
	}

	db := open("spec-start-timeout-migration")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	var seconds int
	if err := db.Raw("SELECT start_timeout_sec FROM inference_specs WHERE id = 1").Scan(&seconds).Error; err != nil || seconds != 0 {
		t.Fatalf("existing spec start_timeout_sec = %d err=%v", seconds, err)
	}
	var null int64
	if err := db.Raw("SELECT COUNT(*) FROM inference_specs WHERE start_timeout_sec IS NULL").Scan(&null).Error; err != nil || null != 0 {
		t.Fatalf("existing specs left NULL = %d err=%v", null, err)
	}
	var marker SchemaMigration
	if err := db.First(&marker, 230).Error; err != nil || marker.Name != "inference_spec_start_timeout" {
		t.Fatalf("marker = %+v err=%v", marker, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}

	rollback := open("spec-start-timeout-migration-rollback")
	want := errors.New("injected spec start_timeout_sec failure")
	failing := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return want
	}}
	if err := applyMigration(rollback, failing); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	if rollback.Migrator().HasColumn(&InferenceSpecRow{}, "StartTimeoutSec") {
		t.Fatal("start_timeout_sec survived the failed migration")
	}
	var count int64
	if err := rollback.Model(&SchemaMigration{}).Where("version = ?", 230).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("marker count = %d err=%v", count, err)
	}
	if err := migrateDB(rollback, []migration{m}); err != nil || !rollback.Migrator().HasColumn(&InferenceSpecRow{}, "StartTimeoutSec") {
		t.Fatalf("restart after rollback: %v", err)
	}
}

// start_timeout_sec is stored on the spec row, read back unchanged, and kept
// when an import of a user spec rewrites the model's other specs.
func TestInferenceSpecStartTimeoutRoundTrip(t *testing.T) {
	st, err := Open("file:inference_spec_start_timeout_round_trip?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	spec := func(name string, seconds int) InferenceSpecRow {
		return InferenceSpecRow{
			Name: name, Scenario: "balanced", Engine: "vllm", Quantization: "bf16", Image: "vllm/vllm-openai:latest",
			WeightSources: `[{"provider":"huggingface","repo_id":"Acme/Slow"}]`, GPUs: `{"vendor":"nvidia"}`, StartTimeoutSec: seconds,
		}
	}
	if err := st.ReconcileInferenceCatalog([]InferenceModel{{
		ModelID: "acme/slow", DisplayTitle: "Slow", Source: "upstream", Architecture: "qwen3", ContextLength: 4096,
		Specs: []InferenceSpecRow{spec("slow", 5400), spec("plain", 0)},
	}}); err != nil {
		t.Fatal(err)
	}
	read := func() map[string]int {
		model, err := st.InferenceModelByModelID("acme/slow")
		if err != nil {
			t.Fatal(err)
		}
		got := map[string]int{}
		for _, row := range model.Specs {
			got[row.Name] = row.StartTimeoutSec
		}
		return got
	}
	if got := read(); len(got) != 2 || got["slow"] != 5400 || got["plain"] != 0 {
		t.Fatalf("start_timeout_sec = %v", got)
	}
	if err := st.ImportUserInferenceCatalog([]InferenceModel{{ModelID: "acme/slow", Specs: []InferenceSpecRow{spec("mine", 600)}}}, false); err != nil {
		t.Fatal(err)
	}
	if got := read(); len(got) != 3 || got["slow"] != 5400 || got["plain"] != 0 || got["mine"] != 600 {
		t.Fatalf("start_timeout_sec after import = %v", got)
	}
}
