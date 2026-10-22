package store

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestGPUPowerLimitMigration(t *testing.T) {
	st, err := Open("file:gpu_power_limit_migration_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	version, err := st.CurrentMigration()
	if err != nil {
		t.Fatal(err)
	}
	if version < 26 {
		t.Fatalf("current migration = %d, want at least 26", version)
	}
	if !st.db.Migrator().HasColumn(&GPU{}, "PowerLimitWatts") {
		t.Fatal("gpus.power_limit_watts column is missing")
	}
	if !st.db.Migrator().HasColumn(&ImageNode{}, "Source") {
		t.Fatal("image_nodes.source column is missing")
	}
}
