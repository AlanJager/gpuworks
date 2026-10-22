package store

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestApplyMigrationRollsBackSchemaAndMarkerOnFailure(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:migration-rollback?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	want := errors.New("injected migration failure")
	err = applyMigration(db, migration{Version: 9001, Name: "rollback-fixture", Apply: func(tx *gorm.DB) error {
		if err := tx.Exec("CREATE TABLE rollback_fixture (id INTEGER PRIMARY KEY)").Error; err != nil {
			return err
		}
		return want
	}})
	if !errors.Is(err, want) {
		t.Fatalf("applyMigration error=%v, want %v", err, want)
	}
	if db.Migrator().HasTable("rollback_fixture") {
		t.Fatal("schema change survived failed migration")
	}
	var marker SchemaMigration
	if err := db.Where("version = ?", 9001).First(&marker).Error; !errors.Is(err, gorm.ErrRecordNotFound) {
		t.Fatalf("migration marker after rollback=%+v err=%v", marker, err)
	}
	retry := migration{Version: 9001, Name: "rollback-fixture", Apply: func(tx *gorm.DB) error {
		return tx.Exec("CREATE TABLE rollback_fixture (id INTEGER PRIMARY KEY)").Error
	}}
	if err := migrateDB(db, []migration{retry}); err != nil {
		t.Fatalf("retry migration after rollback: %v", err)
	}
	if !db.Migrator().HasTable("rollback_fixture") {
		t.Fatal("corrected migration did not recreate schema after rollback")
	}
	if err := db.Where("version = ?", 9001).First(&marker).Error; err != nil {
		t.Fatalf("corrected migration marker missing: %v", err)
	}
}

func TestMigrateDBSkipsAlreadyAppliedVersionIdempotently(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:migration-idempotent?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	var applies int
	m := migration{Version: 9002, Name: "idempotent-fixture", Apply: func(tx *gorm.DB) error {
		applies++
		return tx.Exec("CREATE TABLE IF NOT EXISTS idempotent_fixture (id INTEGER PRIMARY KEY)").Error
	}}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if applies != 1 {
		t.Fatalf("migration apply count=%d, want marker skip after one apply", applies)
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", 9002).Count(&markers).Error; err != nil || markers != 1 {
		t.Fatalf("migration marker count=%d err=%v", markers, err)
	}
}
