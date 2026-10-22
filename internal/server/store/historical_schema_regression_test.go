package store

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func historicalSchemaDB(t *testing.T, statements ...string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "historical.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = sqlDB.Close() })
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db
}

func TestHistoricalJobMigrationsUseJobsTable(t *testing.T) {
	for _, version := range []int{13, 14} {
		t.Run(migrationByVersion(t, version).Name, func(t *testing.T) {
			db := historicalSchemaDB(t,
				`CREATE TABLE jobs (id INTEGER PRIMARY KEY, kind TEXT, message TEXT)`,
				`INSERT INTO jobs(id, kind, message) VALUES(1, 'eval', 'retained')`)
			m := migrationByVersion(t, version)
			boom := errors.New("after historical job migration")
			failing := m
			failing.Apply = func(tx *gorm.DB) error {
				if err := m.Apply(tx); err != nil {
					return err
				}
				return boom
			}
			if err := migrateDB(db, []migration{failing}); !errors.Is(err, boom) {
				t.Fatalf("rollback error=%v", err)
			}
			if db.Migrator().HasColumn("jobs", "spec") {
				t.Fatal("failed job migration retained schema changes")
			}
			if err := migrateDB(db, []migration{m}); err != nil {
				t.Fatal(err)
			}
			if !db.Migrator().HasColumn("jobs", "spec") || db.Migrator().HasTable("legacy_jobs") {
				t.Fatal("job schema was migrated under the wrong table name")
			}
			var message string
			if err := db.Raw(`SELECT message FROM jobs WHERE id = 1`).Scan(&message).Error; err != nil || message != "retained" {
				t.Fatalf("historical message=%q err=%v", message, err)
			}
			if err := migrateDB(db, []migration{m}); err != nil {
				t.Fatal(err)
			}
			var markers int64
			if err := db.Model(&SchemaMigration{}).Where("version = ?", version).Count(&markers).Error; err != nil || markers != 1 {
				t.Fatalf("markers=%d err=%v", markers, err)
			}
		})
	}
}

func TestHistoricalGPUIntegerBooleanPreservesUniqueIndex(t *testing.T) {
	for _, version := range []int{2, 3, 18, 25, 51} {
		m := migrationByVersion(t, version)
		t.Run(m.Name, func(t *testing.T) {
			db := historicalSchemaDB(t,
				`CREATE TABLE nodes (id INTEGER PRIMARY KEY, uuid TEXT)`,
				`CREATE TABLE gpus (id INTEGER PRIMARY KEY, node_id INTEGER, "index" INTEGER, uuid TEXT, mig_enabled INTEGER)`,
				`CREATE UNIQUE INDEX idx_node_gpu ON gpus(node_id, "index")`,
				`INSERT INTO gpus(id, node_id, "index", uuid, mig_enabled) VALUES(1, 1, 0, 'GPU-retained', 1)`)
			boom := errors.New("after historical GPU migration")
			failing := m
			failing.Apply = func(tx *gorm.DB) error {
				if err := m.Apply(tx); err != nil {
					return err
				}
				return boom
			}
			if err := migrateDB(db, []migration{failing}); !errors.Is(err, boom) {
				t.Fatalf("rollback error=%v", err)
			}
			if !db.Migrator().HasIndex("gpus", "idx_node_gpu") || db.Migrator().HasColumn("gpus", "ident") {
				t.Fatal("failed GPU migration changed historical indexes or columns")
			}
			var markers int64
			if err := db.Model(&SchemaMigration{}).Count(&markers).Error; err != nil || markers != 0 {
				t.Fatalf("rollback markers=%d err=%v", markers, err)
			}
			if err := migrateDB(db, []migration{m}); err != nil {
				t.Fatal(err)
			}
			if !db.Migrator().HasIndex("gpus", "idx_node_gpu") {
				t.Fatal("historical GPU unique index lost during boolean column rebuild")
			}
			if err := db.Exec(`INSERT INTO gpus(node_id, "index") VALUES(1, 0)`).Error; err == nil {
				t.Fatal("duplicate historical node/device index accepted")
			}
			var enabled bool
			if err := db.Raw(`SELECT mig_enabled FROM gpus WHERE id = 1`).Scan(&enabled).Error; err != nil || !enabled {
				t.Fatalf("MIG enabled=%v err=%v", enabled, err)
			}
			if err := m.Apply(db); err != nil {
				t.Fatalf("repeat apply: %v", err)
			}
		})
	}
}

func TestHistoricalStoragePathsBeforeOverrideScope(t *testing.T) {
	db := historicalSchemaDB(t,
		`CREATE TABLE storages (id INTEGER PRIMARY KEY, name TEXT, type TEXT, path TEXT, cache_path TEXT, created_at DATETIME, updated_at DATETIME)`,
		`CREATE TABLE storage_nodes (id INTEGER PRIMARY KEY, storage_id INTEGER, node_id INTEGER, label_key TEXT, label_value TEXT, path TEXT, cache_path TEXT, cache_max_bytes INTEGER, existing NUMERIC, created_at DATETIME)`,
		`CREATE UNIQUE INDEX idx_storage_node ON storage_nodes(storage_id, node_id)`,
		`CREATE TABLE storage_node_states (id INTEGER PRIMARY KEY, storage_id INTEGER, node_id INTEGER, path TEXT, cache_path TEXT)`,
		`INSERT INTO storages(id, name, type, path) VALUES(1, 'default', 'local', '')`,
		`INSERT INTO storage_node_states(id, storage_id, node_id, path) VALUES(1, 1, 7, '/historical/models')`)
	m := migrationByVersion(t, 126)
	boom := errors.New("after historical storage path backfill")
	failing := m
	failing.Apply = func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return boom
	}
	if err := migrateDB(db, []migration{failing}); !errors.Is(err, boom) {
		t.Fatalf("rollback error=%v, want %v", err, boom)
	}
	var count int64
	if err := db.Table("storage_nodes").Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("rollback override rows=%d err=%v", count, err)
	}
	if err := db.Model(&SchemaMigration{}).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("rollback markers=%d err=%v", count, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	var path string
	if err := db.Raw(`SELECT path FROM storage_nodes WHERE storage_id = 1 AND node_id = 7`).Scan(&path).Error; err != nil || path != "/historical/models" {
		t.Fatalf("override path=%q err=%v", path, err)
	}
	if db.Migrator().HasColumn("storage_nodes", "override_only") {
		t.Fatal("historical path migration introduced the later override scope column")
	}
	if err := m.Apply(db); err != nil {
		t.Fatal(err)
	}
	if err := db.Table("storage_nodes").Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("idempotent overrides=%d err=%v", count, err)
	}
}

func TestLegacySchemaRepairsAppliedSpecOwnershipAndRetiredTable(t *testing.T) {
	db := historicalSchemaDB(t,
		`CREATE TABLE legacy_jobs (id INTEGER PRIMARY KEY, message TEXT)`,
		`INSERT INTO legacy_jobs(id, message) VALUES(1, 'retired')`,
		`CREATE TABLE inference_models (id INTEGER PRIMARY KEY, source TEXT, package_digest TEXT)`,
		`CREATE TABLE inference_specs (id INTEGER PRIMARY KEY, model_id INTEGER, source TEXT, name TEXT, package_digest TEXT)`,
		`CREATE UNIQUE INDEX idx_inference_spec_identity ON inference_specs(model_id, source, name)`,
		`CREATE TABLE deployments (id INTEGER PRIMARY KEY, template_id INTEGER, template_spec_name TEXT, template_spec_source TEXT, template_snapshot TEXT)`,
		`INSERT INTO deployments(id, template_id, template_spec_name, template_spec_source, template_snapshot) VALUES
		(1, 1, 'misclassified', 'upstream', 'historical launch snapshot'),
		(2, 1, 'official', 'upstream', ''), (3, 1, 'collision', 'upstream', ''), (4, 1, 'misclassified', 'user', '')`,
		`INSERT INTO inference_models(id, source, package_digest) VALUES(1, 'user', 'user-package'), (2, 'upstream', 'official-package'), (3, 'user', '')`,
		`INSERT INTO inference_specs(id, model_id, source, name, package_digest) VALUES
		(1, 1, 'upstream', 'misclassified', 'user-package'),
		(2, 1, 'upstream', 'official', 'other-package'),
		(3, 2, 'user', 'extension', 'user-package'),
		(4, 3, 'upstream', 'unknown', ''),
		(5, 1, 'upstream', 'collision', 'user-package'),
		(6, 1, 'user', 'collision', 'newer-user-package')`)
	boom := errors.New("after legacy schema repair")
	m := migrationByVersion(t, 164)
	m.Apply = func(tx *gorm.DB) error {
		if err := migrateLegacySchemaRepairs(tx); err != nil {
			return err
		}
		return boom
	}
	if err := migrateDB(db, []migration{m}); !errors.Is(err, boom) {
		t.Fatalf("rollback error=%v", err)
	}
	if !db.Migrator().HasTable("legacy_jobs") {
		t.Fatal("failed repair dropped historical table outside its transaction")
	}
	var source string
	if err := db.Raw("SELECT source FROM inference_specs WHERE id = 1").Scan(&source).Error; err != nil || source != "upstream" {
		t.Fatalf("rolled back ownership=%q err=%v", source, err)
	}
	if err := db.Raw("SELECT template_spec_source FROM deployments WHERE id = 1").Scan(&source).Error; err != nil || source != "upstream" {
		t.Fatalf("rolled back deployment ownership=%q err=%v", source, err)
	}
	m.Apply = migrateLegacySchemaRepairs
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if db.Migrator().HasTable("legacy_jobs") {
		t.Fatal("retired accidental jobs table survived repair")
	}
	for id, want := range map[int]string{1: "user", 2: "upstream", 3: "user", 4: "upstream", 5: "upstream", 6: "user"} {
		if err := db.Raw("SELECT source FROM inference_specs WHERE id = ?", id).Scan(&source).Error; err != nil || source != want {
			t.Fatalf("spec %d source=%q err=%v, want %q", id, source, err, want)
		}
	}
	for id, want := range map[int]string{1: "user", 2: "upstream", 3: "upstream", 4: "user"} {
		if err := db.Raw("SELECT template_spec_source FROM deployments WHERE id = ?", id).Scan(&source).Error; err != nil || source != want {
			t.Fatalf("deployment %d source=%q err=%v, want %q", id, source, err, want)
		}
	}
	var snapshot string
	if err := db.Raw("SELECT template_snapshot FROM deployments WHERE id = 1").Scan(&snapshot).Error; err != nil || snapshot != "historical launch snapshot" {
		t.Fatalf("deployment snapshot=%q err=%v", snapshot, err)
	}
	if err := m.Apply(db); err != nil {
		t.Fatalf("repeat apply: %v", err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", m.Version).Count(&markers).Error; err != nil || markers != 1 {
		t.Fatalf("repair markers=%d err=%v", markers, err)
	}
}
