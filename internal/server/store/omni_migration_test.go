package store

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestModelEngineSupportColumnsAreRemoved(t *testing.T) {
	st, err := Open("file:model_engine_support_migration_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	version, err := st.CurrentMigration()
	if err != nil {
		t.Fatal(err)
	}
	if version < 81 {
		t.Fatalf("current migration = %d, want at least 81", version)
	}
	if st.db.Migrator().HasColumn("models", "vllm_support") {
		t.Fatal("models.vllm_support column still exists")
	}
	if st.db.Migrator().HasColumn("models", "engine_support") {
		t.Fatal("models.engine_support column still exists")
	}
	// Exercise the destructive part against a legacy-shaped table as well. The
	// normal Open path starts from the current Model entity, so AutoMigrate alone
	// cannot prove that an upgraded database loses its old columns.
	if err := st.db.Exec("ALTER TABLE models ADD COLUMN vllm_support TEXT").Error; err != nil {
		t.Fatal(err)
	}
	if err := st.db.Exec("ALTER TABLE models ADD COLUMN engine_support TEXT").Error; err != nil {
		t.Fatal(err)
	}
	if err := dropModelEngineSupport(st.db); err != nil {
		t.Fatalf("drop legacy columns: %v", err)
	}
	if err := dropModelEngineSupport(st.db); err != nil {
		t.Fatalf("repeat drop should be idempotent: %v", err)
	}
	if st.db.Migrator().HasColumn("models", "vllm_support") || st.db.Migrator().HasColumn("models", "engine_support") {
		t.Fatal("legacy model engine columns survived the drop")
	}
}
