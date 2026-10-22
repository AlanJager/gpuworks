package store

import (
	"errors"
	"reflect"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// The four tables exactly as store.Open created them at migration 112
// (copied from sqlite_master), indexes included.
var storeSchemaAt112 = []string{
	"CREATE TABLE `image_series` (`id` integer PRIMARY KEY AUTOINCREMENT,`name` text,`engine` text,`purpose` text,`framework` text,`common_args` text,`common_env` text,`vendors` text,`source` text,`deprecated_since` text,`notes` text,`enabled` numeric,`created_at` datetime,`updated_at` datetime)",
	"CREATE INDEX `idx_image_series_enabled` ON `image_series`(`enabled`)",
	"CREATE INDEX `idx_image_series_engine` ON `image_series`(`engine`)",
	"CREATE INDEX `idx_image_series_framework` ON `image_series`(`framework`)",
	"CREATE UNIQUE INDEX `idx_image_series_name` ON `image_series`(`name`)",
	"CREATE INDEX `idx_image_series_purpose` ON `image_series`(`purpose`)",
	"CREATE INDEX `idx_image_series_source` ON `image_series`(`source`)",
	"CREATE TABLE `image_versions` (`id` integer PRIMARY KEY AUTOINCREMENT,`series_id` integer,`name` text,`engine` text,`engine_version` text,`framework` text,`framework_version` text,`variant` text,`arch` text,`image_ref` text,`source` text,`vendors` text,`deprecated` numeric,`enabled` numeric,`size_bytes` integer,`verification_state` text,`verification_error` text,`notes` text,`offline_blob_path` text,`offline_blob_sha256` text,`offline_blob_bytes` integer,`adapted_models` text,`deprecated_since` text,`registry_sources` text,`supported_gp_us` text,`unsupported_gp_us` text,`rolling_tag` numeric,`upstream_gone` numeric,`created_at` datetime,`updated_at` datetime)",
	"CREATE INDEX `idx_image_versions_upstream_gone` ON `image_versions`(`upstream_gone`)",
	"CREATE INDEX `idx_image_versions_rolling_tag` ON `image_versions`(`rolling_tag`)",
	"CREATE INDEX `idx_image_versions_verification_state` ON `image_versions`(`verification_state`)",
	"CREATE INDEX `idx_image_versions_enabled` ON `image_versions`(`enabled`)",
	"CREATE INDEX `idx_image_versions_deprecated` ON `image_versions`(`deprecated`)",
	"CREATE INDEX `idx_image_versions_source` ON `image_versions`(`source`)",
	"CREATE UNIQUE INDEX `idx_image_versions_image_ref` ON `image_versions`(`image_ref`)",
	"CREATE INDEX `idx_image_versions_framework` ON `image_versions`(`framework`)",
	"CREATE INDEX `idx_image_versions_engine` ON `image_versions`(`engine`)",
	"CREATE INDEX `idx_image_versions_series_id` ON `image_versions`(`series_id`)",
	"CREATE TABLE `storages` (`id` integer PRIMARY KEY AUTOINCREMENT,`name` text,`type` text,`enabled` numeric,`builtin` numeric,`config` text,`created_at` datetime,`updated_at` datetime)",
	"CREATE INDEX `idx_storages_builtin` ON `storages`(`builtin`)",
	"CREATE INDEX `idx_storages_enabled` ON `storages`(`enabled`)",
	"CREATE UNIQUE INDEX `idx_storages_name` ON `storages`(`name`)",
	"CREATE TABLE \"network_test_runs\"  (`id` integer PRIMARY KEY AUTOINCREMENT,`network_id` integer,`status` text,`kind` text,`pattern` text,`scope` text,`scope_detail` text,`bandwidth` numeric,`seconds` integer,`nodes` text,`node_count` integer,`pair_count` integer,`passed_count` integer,`pairs` text,`started_at` datetime,`finished_at` datetime)",
}

// openStoreSchemaAt112 is a database migration 112 left behind, with one row
// in each table that 113 and 115 touch (every dropped column holding a value)
// and the network test runs 114 splits: one per shape scope_detail took (and
// one it should never have taken).
func openStoreSchemaAt112(t *testing.T, name string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	statements := append([]string{}, storeSchemaAt112...)
	statements = append(statements,
		`INSERT INTO image_series (id, name, engine, purpose, framework, common_args, common_env, vendors, source, notes, enabled, created_at, updated_at)
			VALUES (3, 'vllm', 'vllm', 'inference', 'cuda', '[{"flag":"--trust-remote-code"}]', '[]', '["nvidia"]', 'custom', 'series notes', 1, '2026-09-01 00:00:00', '2026-09-02 00:00:00')`,
		`INSERT INTO image_versions (id, series_id, name, engine, engine_version, image_ref, source, vendors, enabled, size_bytes, notes, adapted_models, registry_sources, supported_gp_us, unsupported_gp_us, rolling_tag, created_at, updated_at)
			VALUES (1, 3, 'vllm', 'vllm', '0.9.1', 'example/vllm:v1', 'custom', '["nvidia"]', 1, 1024, 'kept notes', '[{"label":"Qwen3-8B"}]', '["https://mirror/a.tar"]', '["H20"]', '["sm70"]', 0, '2026-09-01 00:00:00', '2026-09-02 00:00:00')`,
		`INSERT INTO storages (id, name, type, enabled, builtin, config, created_at, updated_at) VALUES (1, 'local', 'local', 1, 1, '{}', '2026-09-01 00:00:00', '2026-09-01 00:00:00')`,
		`INSERT INTO network_test_runs (id, network_id, status, kind, pattern, scope, scope_detail, bandwidth, seconds, nodes, node_count, pair_count, passed_count, pairs, started_at) VALUES
			(1, 1, 'done', 'tcp', 'mesh', 'label', 'rack=r2', 0, 3, '["c","d"]', 2, 2, 2, '[]', '2026-09-01 00:00:00'),
			(2, 1, 'done', 'tcp', 'mesh', 'label', 'k=a=b', 0, 3, '["c","d"]', 2, 2, 2, '[]', '2026-09-01 00:00:00'),
			(3, 1, 'done', 'tcp', 'mesh', 'label', 'zone=', 0, 3, '["c","d"]', 2, 2, 2, '[]', '2026-09-01 00:00:00'),
			(4, 1, 'done', 'tcp', 'mesh', 'label', 'garbage', 0, 3, '["c","d"]', 2, 2, 2, '[]', '2026-09-01 00:00:00'),
			(5, 1, 'done', 'tcp', 'mesh', 'nodes', '["u1","u2"]', 0, 3, '["u1","u2"]', 2, 2, 2, '[]', '2026-09-01 00:00:00'),
			(6, 1, 'done', 'tcp', 'ring', 'all', '', 1, 3, '["a","b","c"]', 3, 3, 3, '[]', '2026-09-01 00:00:00')`,
	)
	for _, statement := range statements {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	return db
}

// tableRows reads every row of a table as column → value, in id order.
func tableRows(t *testing.T, db *gorm.DB, table string) []map[string]any {
	t.Helper()
	var rows []map[string]any
	if err := db.Raw(`SELECT * FROM "` + table + `" ORDER BY id`).Scan(&rows).Error; err != nil {
		t.Fatal(err)
	}
	return rows
}

// withoutColumns copies rows leaving out the named columns.
func withoutColumns(rows []map[string]any, columns ...string) []map[string]any {
	out := make([]map[string]any, 0, len(rows))
	for _, row := range rows {
		copied := map[string]any{}
		for key, value := range row {
			copied[key] = value
		}
		for _, column := range columns {
			delete(copied, column)
		}
		out = append(out, copied)
	}
	return out
}

func indexNames(t *testing.T, db *gorm.DB, table string) []string {
	t.Helper()
	var names []string
	if err := db.Raw("SELECT name FROM sqlite_master WHERE type = 'index' AND tbl_name = ? ORDER BY name", table).Scan(&names).Error; err != nil {
		t.Fatal(err)
	}
	return names
}

func TestUnusedImageAndStorageColumnsMigration(t *testing.T) {
	m := migrationByVersion(t, 113)
	db := openStoreSchemaAt112(t, "unused-columns-migration")
	images, storages := tableRows(t, db, "image_versions"), tableRows(t, db, "storages")
	imageIndexes, storageIndexes := indexNames(t, db, "image_versions"), indexNames(t, db, "storages")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if columnsOf(t, db, "image_versions")["adapted_models"] || columnsOf(t, db, "storages")["config"] {
		t.Fatalf("dropped columns survived: image_versions=%v storages=%v", columnsOf(t, db, "image_versions"), columnsOf(t, db, "storages"))
	}
	// Every other column keeps its value, and no index is lost.
	if got, want := tableRows(t, db, "image_versions"), withoutColumns(images, "adapted_models"); !reflect.DeepEqual(got, want) {
		t.Fatalf("image rows = %v, want %v", got, want)
	}
	if got, want := tableRows(t, db, "storages"), withoutColumns(storages, "config"); !reflect.DeepEqual(got, want) {
		t.Fatalf("storage rows = %v, want %v", got, want)
	}
	if got := indexNames(t, db, "image_versions"); !reflect.DeepEqual(got, imageIndexes) {
		t.Fatalf("image indexes = %v, want %v", got, imageIndexes)
	}
	if got := indexNames(t, db, "storages"); !reflect.DeepEqual(got, storageIndexes) {
		t.Fatalf("storage indexes = %v, want %v", got, storageIndexes)
	}
	var marker SchemaMigration
	if err := db.First(&marker, 113).Error; err != nil || marker.Name != "unused_image_and_storage_columns_removed" {
		t.Fatalf("marker = %+v err=%v", marker, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
	// Running the body again on the finished schema is a no-op, not an error.
	if err := m.Apply(db); err != nil {
		t.Fatalf("re-applying on the migrated schema: %v", err)
	}
	if got := tableRows(t, db, "image_versions"); !reflect.DeepEqual(got, withoutColumns(images, "adapted_models")) {
		t.Fatalf("re-applying changed image rows: %v", got)
	}
}

func TestUnusedImageAndStorageColumnsMigrationRollsBackWithItsMarker(t *testing.T) {
	m := migrationByVersion(t, 113)
	db := openStoreSchemaAt112(t, "unused-columns-rollback")
	want := errors.New("injected unused columns failure")
	failing := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return want
	}}
	if err := applyMigration(db, failing); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	if !columnsOf(t, db, "image_versions")["adapted_models"] || !columnsOf(t, db, "storages")["config"] {
		t.Fatal("the failed migration left schema changes behind")
	}
	var config string
	if err := db.Raw("SELECT config FROM storages WHERE id = 1").Scan(&config).Error; err != nil || config != "{}" {
		t.Fatalf("storage config = %q err=%v", config, err)
	}
	var count int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", 113).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("marker count = %d err=%v", count, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("restart after rollback: %v", err)
	}
	if columnsOf(t, db, "image_versions")["adapted_models"] || columnsOf(t, db, "storages")["config"] {
		t.Fatal("restart after rollback did not migrate")
	}
}

// 113, 114 and 115 are no-ops where their tables do not exist: none creates one.
func TestStoreTypedColumnMigrationsSkipMissingTables(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:typed-columns-no-tables?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{migrationByVersion(t, 113), migrationByVersion(t, 114), migrationByVersion(t, 115)}); err != nil {
		t.Fatal(err)
	}
	for _, table := range []string{"image_series", "image_versions", "storages", "network_test_runs"} {
		if db.Migrator().HasTable(table) {
			t.Fatalf("%s was created", table)
		}
	}
}

// A fresh database (whose earlier AutoMigrate calls already build the current
// shape) and an upgraded one end with the same columns, and 113, 114 and 115
// do nothing to the fresh one.
func TestStoreTypedColumnsFreshAndUpgradedSchemasConverge(t *testing.T) {
	fresh := newTestStore(t)
	upgraded := openStoreSchemaAt112(t, "typed-columns-upgraded")
	for version := 1; version <= 112; version++ {
		if err := upgraded.Create(&SchemaMigration{Version: version, Name: "applied"}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := migrateDB(upgraded, migrations); err != nil {
		t.Fatal(err)
	}
	for _, version := range []int{113, 114, 115} {
		var count int64
		if err := upgraded.Model(&SchemaMigration{}).Where("version = ?", version).Count(&count).Error; err != nil || count != 1 {
			t.Fatalf("marker %d count = %d err=%v", version, count, err)
		}
	}
	for _, table := range []string{"image_series", "image_versions", "storages", "network_test_runs"} {
		if got, want := columnsOf(t, upgraded, table), columnsOf(t, fresh.db, table); !reflect.DeepEqual(got, want) {
			t.Fatalf("%s: upgraded columns %v, fresh columns %v", table, got, want)
		}
	}
	freshSeries, freshImages, freshStorages, freshRuns := columnsOf(t, fresh.db, "image_series"), columnsOf(t, fresh.db, "image_versions"), columnsOf(t, fresh.db, "storages"), columnsOf(t, fresh.db, "network_test_runs")
	if freshImages["adapted_models"] || freshStorages["config"] || freshRuns["scope_detail"] || !freshRuns["label_key"] || !freshRuns["label_value"] {
		t.Fatalf("fresh columns: image_versions=%v storages=%v network_test_runs=%v", freshImages, freshStorages, freshRuns)
	}
	if freshSeries["vendors"] || freshImages["vendors"] || freshImages["supported_gp_us"] || freshImages["unsupported_gp_us"] {
		t.Fatalf("fresh columns: image_series=%v image_versions=%v", freshSeries, freshImages)
	}
	for _, version := range []int{113, 114, 115} {
		if err := migrationByVersion(t, version).Apply(fresh.db); err != nil {
			t.Fatalf("migration %d on a fresh database: %v", version, err)
		}
	}
	if !reflect.DeepEqual(columnsOf(t, fresh.db, "network_test_runs"), freshRuns) || !reflect.DeepEqual(columnsOf(t, fresh.db, "image_series"), freshSeries) || !reflect.DeepEqual(columnsOf(t, fresh.db, "image_versions"), freshImages) || !reflect.DeepEqual(columnsOf(t, fresh.db, "storages"), freshStorages) {
		t.Fatal("re-applying 113, 114 or 115 changed a fresh schema")
	}
}

// adaptationColumnsPresent lists which of the four columns 115 drops are on
// the database.
func adaptationColumnsPresent(t *testing.T, db *gorm.DB) []string {
	t.Helper()
	var present []string
	if columnsOf(t, db, "image_series")["vendors"] {
		present = append(present, "image_series.vendors")
	}
	for _, column := range []string{"vendors", "supported_gp_us", "unsupported_gp_us"} {
		if columnsOf(t, db, "image_versions")[column] {
			present = append(present, "image_versions."+column)
		}
	}
	return present
}

var adaptationColumns = []string{"image_series.vendors", "image_versions.vendors", "image_versions.supported_gp_us", "image_versions.unsupported_gp_us"}

func TestUnusedImageAdaptationColumnsMigration(t *testing.T) {
	m := migrationByVersion(t, 115)
	db := openStoreSchemaAt112(t, "unused-adaptation-columns-migration")
	if got := adaptationColumnsPresent(t, db); !reflect.DeepEqual(got, adaptationColumns) {
		t.Fatalf("fixture columns = %v", got)
	}
	series, images := tableRows(t, db, "image_series"), tableRows(t, db, "image_versions")
	seriesIndexes, imageIndexes := indexNames(t, db, "image_series"), indexNames(t, db, "image_versions")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if got := adaptationColumnsPresent(t, db); len(got) != 0 {
		t.Fatalf("dropped columns survived: %v", got)
	}
	// Every other column keeps its value, and no index is lost.
	wantSeries, wantImages := withoutColumns(series, "vendors"), withoutColumns(images, "vendors", "supported_gp_us", "unsupported_gp_us")
	if got := tableRows(t, db, "image_series"); !reflect.DeepEqual(got, wantSeries) {
		t.Fatalf("series rows = %v, want %v", got, wantSeries)
	}
	if got := tableRows(t, db, "image_versions"); !reflect.DeepEqual(got, wantImages) {
		t.Fatalf("image rows = %v, want %v", got, wantImages)
	}
	if got := indexNames(t, db, "image_series"); !reflect.DeepEqual(got, seriesIndexes) {
		t.Fatalf("series indexes = %v, want %v", got, seriesIndexes)
	}
	if got := indexNames(t, db, "image_versions"); !reflect.DeepEqual(got, imageIndexes) {
		t.Fatalf("image indexes = %v, want %v", got, imageIndexes)
	}
	// The entities read the migrated rows back.
	var row Image
	if err := db.First(&row, 1).Error; err != nil || row.ImageRef != "example/vllm:v1" || row.Notes != "kept notes" || row.RegistrySources != `["https://mirror/a.tar"]` {
		t.Fatalf("image = %+v err=%v", row, err)
	}
	var seriesRow ImageSeries
	if err := db.First(&seriesRow, 3).Error; err != nil || seriesRow.Name != "vllm" || seriesRow.CommonArgs != `[{"flag":"--trust-remote-code"}]` || seriesRow.Notes != "series notes" {
		t.Fatalf("series = %+v err=%v", seriesRow, err)
	}
	var marker SchemaMigration
	if err := db.First(&marker, 115).Error; err != nil || marker.Name != "unused_image_adaptation_columns_removed" {
		t.Fatalf("marker = %+v err=%v", marker, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
	// Running the body again on the finished schema is a no-op, not an error.
	if err := m.Apply(db); err != nil {
		t.Fatalf("re-applying on the migrated schema: %v", err)
	}
	if got := tableRows(t, db, "image_versions"); !reflect.DeepEqual(got, wantImages) {
		t.Fatalf("re-applying changed image rows: %v", got)
	}
	if got := tableRows(t, db, "image_series"); !reflect.DeepEqual(got, wantSeries) {
		t.Fatalf("re-applying changed series rows: %v", got)
	}
}

func TestUnusedImageAdaptationColumnsMigrationRollsBackWithItsMarker(t *testing.T) {
	m := migrationByVersion(t, 115)
	db := openStoreSchemaAt112(t, "unused-adaptation-columns-rollback")
	series, images := tableRows(t, db, "image_series"), tableRows(t, db, "image_versions")
	want := errors.New("injected adaptation columns failure")
	failing := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return want
	}}
	if err := applyMigration(db, failing); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	if got := adaptationColumnsPresent(t, db); !reflect.DeepEqual(got, adaptationColumns) {
		t.Fatalf("the failed migration left schema changes behind: %v", got)
	}
	if got := tableRows(t, db, "image_series"); !reflect.DeepEqual(got, series) {
		t.Fatalf("series rows = %v, want %v", got, series)
	}
	if got := tableRows(t, db, "image_versions"); !reflect.DeepEqual(got, images) {
		t.Fatalf("image rows = %v, want %v", got, images)
	}
	var count int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", 115).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("marker count = %d err=%v", count, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("restart after rollback: %v", err)
	}
	if got := adaptationColumnsPresent(t, db); len(got) != 0 {
		t.Fatalf("restart after rollback did not migrate: %v", got)
	}
}
