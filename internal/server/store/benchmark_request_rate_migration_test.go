package store

import (
	"errors"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestBenchmarkPointRequestRateMigration(t *testing.T) {
	open := func(name string) *gorm.DB {
		db, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"), &gorm.Config{})
		if err != nil {
			t.Fatal(err)
		}
		if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("CREATE TABLE benchmark_points (id INTEGER PRIMARY KEY, run_id INTEGER, seq INTEGER, concurrency INTEGER, output_tput REAL)").Error; err != nil {
			t.Fatal(err)
		}
		if err := db.Exec("INSERT INTO benchmark_points (id, run_id, seq, concurrency, output_tput) VALUES (1, 7, 1, 4, 120.5)").Error; err != nil {
			t.Fatal(err)
		}
		return db
	}
	m := migrationByVersion(t, 134)

	db := open("benchmark-point-request-rate")
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatal(err)
	}
	if !db.Migrator().HasColumn(&BenchmarkPoint{}, "RequestRate") {
		t.Fatal("request_rate was not added")
	}
	// Every existing point is closed loop.
	var point struct {
		Concurrency int
		RequestRate float64
		OutputTput  float64
	}
	if err := db.Raw("SELECT concurrency, request_rate, output_tput FROM benchmark_points WHERE id = 1").Scan(&point).Error; err != nil || point.Concurrency != 4 || point.RequestRate != 0 || point.OutputTput != 120.5 {
		t.Fatalf("existing point = %+v err=%v", point, err)
	}
	// The column is NOT NULL DEFAULT 0, so inserts that predate it still work.
	if err := db.Exec("INSERT INTO benchmark_points (id, run_id, seq, concurrency) VALUES (2, 7, 2, 8)").Error; err != nil {
		t.Fatal(err)
	}
	var marker SchemaMigration
	if err := db.First(&marker, 134).Error; err != nil || marker.Name != "benchmark_point_request_rate" {
		t.Fatalf("marker = %+v err=%v", marker, err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("not idempotent: %v", err)
	}
	if err := db.Exec("DELETE FROM schema_migrations WHERE version = 134").Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{m}); err != nil {
		t.Fatalf("rerun without marker: %v", err)
	}

	rollback := open("benchmark-point-request-rate-rollback")
	want := errors.New("injected request_rate failure")
	failing := migration{Version: m.Version, Name: m.Name, Apply: func(tx *gorm.DB) error {
		if err := m.Apply(tx); err != nil {
			return err
		}
		return want
	}}
	if err := applyMigration(rollback, failing); !errors.Is(err, want) {
		t.Fatalf("error = %v", err)
	}
	if rollback.Migrator().HasColumn(&BenchmarkPoint{}, "RequestRate") {
		t.Fatal("the column survived the failed migration")
	}
	var count int64
	if err := rollback.Model(&SchemaMigration{}).Where("version = ?", 134).Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("marker count = %d err=%v", count, err)
	}
	if err := migrateDB(rollback, []migration{m}); err != nil || !rollback.Migrator().HasColumn(&BenchmarkPoint{}, "RequestRate") {
		t.Fatalf("restart after rollback: err=%v", err)
	}
}
