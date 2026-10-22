package store

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func openInstanceRoleDB(t *testing.T, name string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TABLE instances (id INTEGER PRIMARY KEY, uuid TEXT)").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO instances (id, uuid) VALUES (1, 'old')").Error; err != nil {
		t.Fatal(err)
	}
	return db
}

func TestInstanceRoleMigrationIsIdempotent(t *testing.T) {
	db := openInstanceRoleDB(t, "instance-role-migration")
	m := migrationByVersion(t, 108)
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasColumn(&Instance{}, "Role") {
		t.Fatal("instances.role was not added")
	}
	var role string
	if err := db.Raw("SELECT role FROM instances WHERE id = 1").Scan(&role).Error; err != nil || role != "" {
		t.Fatalf("existing row role = %q, err=%v", role, err)
	}
	var marker SchemaMigration
	if err := db.First(&marker, 108).Error; err != nil || marker.Name != "instance_pd_role" {
		t.Fatalf("marker = %+v, err=%v", marker, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
	// A second run with the marker removed still succeeds: the column check
	// guards a restart after a partially recorded run.
	if err := db.Exec("DELETE FROM schema_migrations WHERE version = 108").Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("rerun without marker: %v", err)
	}
}

func TestInstanceRoleMigrationRollsBackSchemaAndMarker(t *testing.T) {
	db := openInstanceRoleDB(t, "instance-role-migration-rollback")
	base := migrationByVersion(t, 108)
	want := errors.New("injected instance role failure")
	failing := migration{Version: base.Version, Name: base.Name, Apply: func(tx *gorm.DB) error {
		if err := base.Apply(tx); err != nil {
			return err
		}
		return want
	}}
	if err := applyMigration(db, failing); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	if db.Migrator().HasColumn(&Instance{}, "Role") {
		t.Fatal("role column survived the failed migration")
	}
	var count int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", 108).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("marker count = %d, err=%v", count, err)
	}
	// The next start applies it cleanly.
	if err := migrateDB(db, []migration{base}); err != nil || !db.Migrator().HasColumn(&Instance{}, "Role") {
		t.Fatalf("restart after rollback: %v", err)
	}
}
