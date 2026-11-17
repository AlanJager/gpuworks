package store

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestInferenceSpecPDRolesManualCommandAndDockerArgsMigration(t *testing.T) {
	open := func(name string) *gorm.DB {
		db, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"), &gorm.Config{})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
			t.Fatal(err)
		}
		// A spec stored before the migration: a PD-only spec with its mode.
		if err := db.Exec("CREATE TABLE inference_specs (id INTEGER PRIMARY KEY, name TEXT, pd_mode TEXT)").Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("INSERT INTO inference_specs (id, name, pd_mode) VALUES (1, 'bf16-pd', 'vllm-nixl')").Error; err != nil {
			t.Fatal(err)
		}
		return db
	}
	m := migrationByVersion(t, 220)
	if m.Name != "inference_spec_pd_roles_manual_command_docker_args" {
		t.Fatalf("migration 220 = %q", m.Name)
	}
	db := open("spec-pd-roles-migration")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	var row struct {
		PDMode, PDRoles, ManualCommand, DockerArgs string
	}
	if err := db.Raw("SELECT pd_mode, pd_roles, manual_command, docker_args FROM inference_specs WHERE id = 1").Scan(&row).Error; err != nil {
		t.Fatal(err)
	}
	if row.PDMode != "vllm-nixl" || row.PDRoles != "" || row.ManualCommand != "" || row.DockerArgs != "" {
		t.Fatalf("existing spec = %+v", row)
	}
	var null int64
	if err := db.Raw("SELECT COUNT(*) FROM inference_specs WHERE pd_roles IS NULL OR manual_command IS NULL OR docker_args IS NULL").Scan(&null).Error; err != nil || null != 0 {
		t.Fatalf("existing specs left NULL = %d err=%v", null, err)
	}
	var marker SchemaMigration
	if err := db.First(&marker, 220).Error; err != nil || marker.Name != m.Name {
		t.Fatalf("marker = %+v err=%v", marker, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}

	rollback := open("spec-pd-roles-migration-rollback")
	want := errors.New("injected spec pd_roles failure")
	failing := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return want
	}}
	if err := applyMigration(rollback, failing); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	for _, field := range []string{"PDRoles", "ManualCommand", "DockerArgs"} {
		if rollback.Migrator().HasColumn(&InferenceSpecRow{}, field) {
			t.Fatalf("%s survived the failed migration", field)
		}
	}
	var count int64
	if err := rollback.Model(&SchemaMigration{}).Where("version = ?", 220).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("marker count = %d err=%v", count, err)
	}
	if err := migrateDB(rollback, []migration{m}); err != nil || !rollback.Migrator().HasColumn(&InferenceSpecRow{}, "PDRoles") || !rollback.Migrator().HasColumn(&InferenceSpecRow{}, "ManualCommand") || !rollback.Migrator().HasColumn(&InferenceSpecRow{}, "DockerArgs") {
		t.Fatalf("restart after rollback: %v", err)
	}
}
