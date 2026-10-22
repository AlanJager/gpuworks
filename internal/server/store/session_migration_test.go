package store

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestManagementSessionMigrationSchemaMarkerAndIdempotence(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()
	m := migrationByVersion(t, 140)
	for i := 0; i < 2; i++ {
		if err := migrateDB(db, []migration{m}); err != nil {
			t.Fatal(err)
		}
	}
	for _, field := range []string{"token_hash", "user_id", "created_at", "expires_at", "absolute_expires_at"} {
		if !db.Migrator().HasColumn(&ManagementSession{}, field) {
			t.Fatalf("missing %s", field)
		}
	}
	if !db.Migrator().HasIndex(&ManagementSession{}, "UserID") {
		t.Fatal("user index missing")
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", 140).Count(&markers).Error; err != nil || markers != 1 {
		t.Fatalf("markers=%d err=%v", markers, err)
	}
	row := &ManagementSession{TokenHash: "same-hash", UserID: 1}
	if err := db.Create(row).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&ManagementSession{TokenHash: "same-hash", UserID: 2}).Error; err == nil {
		t.Fatal("token hash not unique")
	}
}

func TestManagementSessionMigrationRollsBackSchemaAndMarker(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(":memory:"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	sqlDB, _ := db.DB()
	defer sqlDB.Close()
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	base := migrationByVersion(t, 140)
	want := errors.New("injected management session migration failure")
	failure := migration{Version: 140, Name: base.Name, Apply: func(tx *gorm.DB) error {
		if err := base.Apply(tx); err != nil {
			return err
		}
		return want
	}}
	if err := applyMigration(db, failure); !errors.Is(err, want) {
		t.Fatalf("migration err=%v", err)
	}
	if db.Migrator().HasTable(&ManagementSession{}) {
		t.Fatal("failed migration left session table behind")
	}
	var markers int64
	if err := db.Model(&SchemaMigration{}).Where("version = ?", 140).Count(&markers).Error; err != nil || markers != 0 {
		t.Fatalf("markers=%d err=%v", markers, err)
	}
	if err := migrateDB(db, []migration{base}); err != nil {
		t.Fatalf("retry after rollback: %v", err)
	}
	if !db.Migrator().HasTable(&ManagementSession{}) {
		t.Fatal("retry failed to create table")
	}
}
