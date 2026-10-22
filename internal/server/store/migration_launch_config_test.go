package store

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// The tables as migration 110 left them: image rows with the launch-default
// columns (one of them indexed), instances with the snapshot and override
// copies, deployments with the RunSpec-era override column.
func openLaunchConfigSchema(t *testing.T, name string, withNewDeploymentColumn bool) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	deploymentColumns := "id INTEGER PRIMARY KEY, name TEXT, run_spec_overrides TEXT"
	if withNewDeploymentColumn {
		deploymentColumns += ", container_overrides TEXT"
	}
	for _, statement := range []string{
		`CREATE TABLE image_versions (id INTEGER PRIMARY KEY, image_ref TEXT, env TEXT, docker_args TEXT, run_command TEXT, model_mount_path TEXT, extra_mounts TEXT, entrypoint_display TEXT, cmd_display TEXT, inspect_synced NUMERIC, command_mode TEXT, registry_sources TEXT)`,
		`CREATE INDEX idx_image_versions_command_mode ON image_versions(command_mode)`,
		`INSERT INTO image_versions (id, image_ref, env, command_mode, registry_sources) VALUES (1, 'example/vllm:v1', '{"HF_HUB_OFFLINE":"1"}', 'engine', '["https://mirror/a.tar"]')`,
		`CREATE TABLE instances (id INTEGER PRIMARY KEY, uuid TEXT, spec TEXT, image_defaults_snapshot TEXT, run_spec_overrides TEXT)`,
		`INSERT INTO instances (id, uuid, spec, image_defaults_snapshot, run_spec_overrides) VALUES (1, 'inst-1', '{"launch":{}}', '{"model_mount_path":"/model"}', '{}')`,
		`CREATE TABLE deployments (` + deploymentColumns + `)`,
		`INSERT INTO deployments (id, name, run_spec_overrides) VALUES (1, 'dep', '{"shm_bytes":34359738368}')`,
	} {
		if err := db.Exec(statement).Error; err != nil {
			t.Fatalf("%s: %v", statement, err)
		}
	}
	return db
}

func columnsOf(t *testing.T, db *gorm.DB, table string) map[string]bool {
	t.Helper()
	var names []string
	if err := db.Raw("SELECT name FROM pragma_table_info(?)", table).Scan(&names).Error; err != nil {
		t.Fatal(err)
	}
	out := map[string]bool{}
	for _, name := range names {
		out[name] = true
	}
	return out
}

func TestImageCatalogLaunchConfigMigration(t *testing.T) {
	m := migrationByVersion(t, 112)
	db := openLaunchConfigSchema(t, "launch-config-migration", false)
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	images := columnsOf(t, db, "image_versions")
	for _, gone := range []string{"env", "docker_args", "run_command", "model_mount_path", "extra_mounts", "entrypoint_display", "cmd_display", "inspect_synced", "command_mode"} {
		if images[gone] {
			t.Fatalf("image_versions.%s survived: %v", gone, images)
		}
	}
	if !images["image_ref"] || !images["registry_sources"] {
		t.Fatalf("image_versions lost acquisition columns: %v", images)
	}
	var sources string
	if err := db.Raw("SELECT registry_sources FROM image_versions WHERE id = 1").Scan(&sources).Error; err != nil || sources != `["https://mirror/a.tar"]` {
		t.Fatalf("registry_sources = %q err=%v", sources, err)
	}
	var indexCount int64
	if err := db.Raw("SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_image_versions_command_mode'").Scan(&indexCount).Error; err != nil || indexCount != 0 {
		t.Fatalf("command_mode index count = %d err=%v", indexCount, err)
	}
	instances := columnsOf(t, db, "instances")
	if instances["image_defaults_snapshot"] || instances["run_spec_overrides"] || !instances["spec"] {
		t.Fatalf("instances columns = %v", instances)
	}
	deployments := columnsOf(t, db, "deployments")
	if deployments["run_spec_overrides"] || !deployments["container_overrides"] {
		t.Fatalf("deployments columns = %v", deployments)
	}
	var overrides string
	if err := db.Raw("SELECT container_overrides FROM deployments WHERE id = 1").Scan(&overrides).Error; err != nil || overrides != `{"shm_bytes":34359738368}` {
		t.Fatalf("renamed overrides = %q err=%v", overrides, err)
	}
	var marker SchemaMigration
	if err := db.First(&marker, 112).Error; err != nil || marker.Name != "image_catalog_launch_config_removed" {
		t.Fatalf("marker = %+v err=%v", marker, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
	// Running the body again on the finished schema is a no-op, not an error.
	if err := m.Apply(db); err != nil {
		t.Fatalf("re-applying on the migrated schema: %v", err)
	}
}

// A database whose baseline re-ran on upgrade has both override columns: the
// old column's values fill the new one where it is empty, then it is dropped.
func TestImageCatalogLaunchConfigMigrationMergesBothOverrideColumns(t *testing.T) {
	db := openLaunchConfigSchema(t, "launch-config-both-columns", true)
	if err := db.Exec(`INSERT INTO deployments (id, name, run_spec_overrides, container_overrides) VALUES (2, 'kept', '{"env":{"OLD":"1"}}', '{"env":{"NEW":"1"}}')`).Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{migrationByVersion(t, 112)}); err != nil {
		t.Fatal(err)
	}
	if columnsOf(t, db, "deployments")["run_spec_overrides"] {
		t.Fatal("run_spec_overrides survived")
	}
	want := map[int]string{1: `{"shm_bytes":34359738368}`, 2: `{"env":{"NEW":"1"}}`}
	for id, expected := range want {
		var got string
		if err := db.Raw("SELECT container_overrides FROM deployments WHERE id = ?", id).Scan(&got).Error; err != nil || got != expected {
			t.Fatalf("deployment %d overrides = %q err=%v, want %q", id, got, err, expected)
		}
	}
}

func TestImageCatalogLaunchConfigMigrationRollsBackWithItsMarker(t *testing.T) {
	m := migrationByVersion(t, 112)
	db := openLaunchConfigSchema(t, "launch-config-rollback", false)
	want := errors.New("injected launch config failure")
	failing := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return want
	}}
	if err := applyMigration(db, failing); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	if !columnsOf(t, db, "image_versions")["command_mode"] || !columnsOf(t, db, "instances")["image_defaults_snapshot"] || !columnsOf(t, db, "deployments")["run_spec_overrides"] {
		t.Fatal("the failed migration left schema changes behind")
	}
	var indexCount int64
	if err := db.Raw("SELECT COUNT(*) FROM sqlite_master WHERE type = 'index' AND name = 'idx_image_versions_command_mode'").Scan(&indexCount).Error; err != nil || indexCount != 1 {
		t.Fatalf("index dropped by the failed migration: count=%d err=%v", indexCount, err)
	}
	var count int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", 112).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("marker count = %d err=%v", count, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("restart after rollback: %v", err)
	}
	if columnsOf(t, db, "image_versions")["command_mode"] || !columnsOf(t, db, "deployments")["container_overrides"] {
		t.Fatal("restart after rollback did not migrate")
	}
}
