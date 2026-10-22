package store

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestDeploymentRoutingPolicyMigrationIsIdempotent(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:routing-policy-migration?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TABLE deployments (id INTEGER PRIMARY KEY)").Error; err != nil {
		t.Fatal(err)
	}
	m := migrationByVersion(t, 94)
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasColumn(&Deployment{}, "RoutingPolicy") {
		t.Fatal("deployments.routing_policy was not added")
	}
	var marker SchemaMigration
	if err := db.First(&marker, 94).Error; err != nil || marker.Name != "deployment_routing_policy" {
		t.Fatalf("migration marker = %+v, err=%v", marker, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("migration is not idempotent: %v", err)
	}
}

func TestDeploymentRoutingPolicyMigrationRollsBackSchemaAndMarker(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:routing-policy-migration-rollback?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE TABLE deployments (id INTEGER PRIMARY KEY)").Error; err != nil {
		t.Fatal(err)
	}
	base := migrationByVersion(t, 94)
	want := errors.New("injected routing migration failure")
	failing := migration{Version: base.Version, Name: base.Name, Apply: func(tx *gorm.DB) error {
		if err := base.Apply(tx); err != nil {
			return err
		}
		return want
	}}
	if err := applyMigration(db, failing); !errors.Is(err, want) {
		t.Fatalf("migration error=%v, want %v", err, want)
	}
	if db.Migrator().HasColumn(&Deployment{}, "RoutingPolicy") {
		t.Fatal("routing_policy column survived failed migration")
	}
	var count int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", 94).Count(&count).Error; err != nil {
		t.Fatal(err)
	}
	if count != 0 {
		t.Fatalf("failed migration marker count=%d, want 0", count)
	}
}
