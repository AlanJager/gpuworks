package store

import (
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
)

// storageMigrations are 122-125 as registered, run against a schema shaped
// like a database from before storages.
func storageMigrations() []migration {
	return []migration{
		{Version: 122, Name: "storage_first_class", Apply: migrateStorageFirstClass},
		{Version: 123, Name: "model_file_storage", Apply: migrateModelFileStorage},
		{Version: 124, Name: "deployment_weight_storage", Apply: migrateDeploymentWeightStorage},
		{Version: 125, Name: "drop_models_root_and_builtin", Apply: dropModelsRootAndBuiltin},
	}
}

func legacyStorageDB(t *testing.T, withBuiltin bool) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		"CREATE TABLE storages (id INTEGER PRIMARY KEY, name TEXT UNIQUE, type TEXT, enabled NUMERIC, builtin NUMERIC, created_at DATETIME, updated_at DATETIME)",
		"CREATE TABLE nodes (id INTEGER PRIMARY KEY, uuid TEXT, models_root TEXT, labels TEXT)",
		"CREATE TABLE model_files (id INTEGER PRIMARY KEY, model_id INTEGER, node_id INTEGER, path TEXT, state TEXT, size_bytes INTEGER)",
		"CREATE UNIQUE INDEX idx_mf ON model_files(model_id, node_id)",
		"CREATE TABLE deployments (id INTEGER PRIMARY KEY, name TEXT, model_id INTEGER)",
		"INSERT INTO nodes (id, uuid, models_root) VALUES (1, 'n1', '/data/models'), (2, 'n2', ''), (3, 'n3', NULL)",
		"INSERT INTO model_files (id, model_id, node_id, path, state) VALUES (1, 7, 1, '/data/models/huggingface/x-main', 'present'), (2, 7, 2, '/opt/registered', 'registered')",
		"INSERT INTO deployments (id, name, model_id) VALUES (1, 'd', 7)",
	} {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatal(err)
		}
	}
	if withBuiltin {
		if err := db.Exec("INSERT INTO storages (id, name, type, enabled, builtin) VALUES (1, 'Local Storage', 'local', 0, 1)").Error; err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestStorageMigrationsTurnTheBuiltinRowIntoTheDefaultLocalStorage(t *testing.T) {
	db := legacyStorageDB(t, true)
	if err := migrateDB(db, storageMigrations()); err != nil {
		t.Fatal(err)
	}
	var storages []Storage
	if err := db.Find(&storages).Error; err != nil {
		t.Fatal(err)
	}
	if len(storages) != 1 {
		t.Fatalf("storages = %+v, want only the migrated row", storages)
	}
	s := storages[0]
	// A disabled built-in row stays disabled: the operator's choice survives.
	if s.ID != 1 || s.Name != defaultStorageName || s.Type != proto.StorageLocal || s.Enabled || !s.IsDefault || !s.AllNodes || s.Path != "" {
		t.Fatalf("migrated storage = %+v", s)
	}
	var targets []StorageNode
	if err := db.Find(&targets).Error; err != nil {
		t.Fatal(err)
	}
	if len(targets) != 1 || targets[0].NodeID != 1 || targets[0].Path != "/data/models" || targets[0].StorageID != 1 {
		t.Fatalf("node overrides = %+v, want node 1's models root only", targets)
	}
	var files []ModelFile
	if err := db.Order("id").Find(&files).Error; err != nil {
		t.Fatal(err)
	}
	if len(files) != 2 || files[0].StorageID != 1 || files[1].StorageID != 1 || files[1].Path != "/opt/registered" {
		t.Fatalf("model files = %+v, want both in the default storage with paths unchanged", files)
	}
	var saveTo uint
	if err := db.Raw("SELECT weight_storage_id FROM deployments WHERE id = 1").Row().Scan(&saveTo); err != nil || saveTo != 1 {
		t.Fatalf("deployment storage = %d err=%v", saveTo, err)
	}
	for table, column := range map[string]string{"nodes": "models_root", "storages": "builtin"} {
		if has, err := tableHasColumn(db, table, column); err != nil || has {
			t.Fatalf("%s.%s still present (err=%v)", table, column, err)
		}
	}
	// Model files are keyed by (model, storage, node) now: the same model and
	// node in a second storage is a second row, the same triple is refused.
	if err := db.Create(&ModelFile{ModelID: 7, StorageID: 2, NodeID: 1, Path: "/nvme/x"}).Error; err != nil {
		t.Fatalf("second storage row: %v", err)
	}
	if err := db.Create(&ModelFile{ModelID: 7, StorageID: 1, NodeID: 1, Path: "/again"}).Error; err == nil {
		t.Fatal("duplicate (model, storage, node) accepted")
	}
	// A shared storage holds one directory per path.
	if err := db.Create(&ModelFile{ModelID: 8, StorageID: 3, NodeID: 0, Path: "Qwen/Qwen3-8B"}).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&ModelFile{ModelID: 9, StorageID: 3, NodeID: 0, Path: "Qwen/Qwen3-8B"}).Error; err == nil {
		t.Fatal("two shared rows at one path accepted")
	}
	// Running the list again changes nothing (markers skip every step).
	if err := migrateDB(db, storageMigrations()); err != nil {
		t.Fatal(err)
	}
	var count int64
	if err := db.Model(&Storage{}).Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("storages after rerun = %d err=%v", count, err)
	}
}

func TestStorageMigrationSeedsADefaultLocalStorageWhenNoneExists(t *testing.T) {
	db := legacyStorageDB(t, false)
	if err := migrateDB(db, storageMigrations()); err != nil {
		t.Fatal(err)
	}
	var storages []Storage
	if err := db.Find(&storages).Error; err != nil {
		t.Fatal(err)
	}
	if len(storages) != 1 || storages[0].Name != defaultStorageName || !storages[0].Enabled || !storages[0].IsDefault || !storages[0].AllNodes {
		t.Fatalf("seeded storages = %+v", storages)
	}
}

func TestStorageMigrationsAreIdempotentWhenRunTwiceWithoutMarkers(t *testing.T) {
	db := legacyStorageDB(t, true)
	for i := 0; i < 2; i++ {
		for _, m := range storageMigrations() {
			if err := db.Transaction(m.Apply); err != nil {
				t.Fatalf("pass %d migration %d: %v", i, m.Version, err)
			}
		}
	}
	var targets int64
	if err := db.Model(&StorageNode{}).Count(&targets).Error; err != nil || targets != 1 {
		t.Fatalf("node overrides after two passes = %d err=%v", targets, err)
	}
}
