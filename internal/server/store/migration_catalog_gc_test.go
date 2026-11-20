package store

import (
	"errors"
	"path/filepath"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

func TestCatalogGCBarrierMigrationAtomicAndIdempotent(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "migration.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	verifyCatalogGCBarrierMigration(t, db)
}

func verifyCatalogGCBarrierMigration(t *testing.T, db *gorm.DB) {
	t.Helper()
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	var change migration
	for _, m := range migrations {
		if m.Version == 245 {
			change = m
		}
	}
	if change.Apply == nil {
		t.Fatal("catalog GC migration missing")
	}
	if err := db.Exec(`CREATE TRIGGER reject_gc_marker BEFORE INSERT ON schema_migrations WHEN NEW.version = 245 BEGIN SELECT RAISE(ABORT, 'injected marker failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	if err := applyMigration(db, change); err == nil {
		t.Fatal("marker failure ignored")
	}
	if db.Migrator().HasTable("catalog_gc_barriers") {
		t.Fatal("table persisted without migration marker")
	}
	var count int64
	if err := db.Model(&SchemaMigration{}).Where("version = 245").Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("failed marker persisted: %d %v", count, err)
	}
	if err := db.Exec("DROP TRIGGER reject_gc_marker").Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{change}); err != nil {
		t.Fatal(err)
	}
	var sequence int64
	if err := db.Raw("SELECT sequence FROM catalog_gc_barriers WHERE id = 1").Scan(&sequence).Error; err != nil || sequence != 0 {
		t.Fatalf("invalid seed %d %v", sequence, err)
	}
	if err := db.Exec("UPDATE catalog_gc_barriers SET sequence = 7 WHERE id = 1").Error; err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{change}); err != nil {
		t.Fatal(err)
	}
	if err := db.Raw("SELECT sequence FROM catalog_gc_barriers WHERE id = 1").Scan(&sequence).Error; err != nil || sequence != 7 {
		t.Fatalf("rerun reset barrier %d %v", sequence, err)
	}
	if err := db.Model(&SchemaMigration{}).Where("version = 245").Count(&count).Error; err != nil || count != 1 {
		t.Fatalf("marker count=%d err=%v", count, err)
	}
	for _, sql := range []string{
		"INSERT INTO catalog_gc_barriers(id,sequence) VALUES(2,0)",
		"INSERT INTO catalog_gc_barriers(id,sequence) VALUES(1,0)",
		"UPDATE catalog_gc_barriers SET sequence = -1",
		"UPDATE catalog_gc_barriers SET sequence = NULL",
		"UPDATE catalog_gc_barriers SET sequence = 1.5",
		"UPDATE catalog_gc_barriers SET sequence = 'invalid'",
	} {
		if err := db.Exec(sql).Error; err == nil {
			t.Fatalf("invalid singleton accepted: %s", sql)
		}
	}
	var columns []struct {
		Name    string
		Type    string
		Notnull int
		Pk      int
	}
	if err := db.Raw("PRAGMA table_info(catalog_gc_barriers)").Scan(&columns).Error; err != nil {
		t.Fatal(err)
	}
	if len(columns) != 2 || columns[0].Name != "id" || columns[0].Type != "INTEGER" || columns[0].Pk != 1 || columns[1].Name != "sequence" || columns[1].Type != "INTEGER" || columns[1].Notnull != 1 {
		t.Fatalf("schema=%+v", columns)
	}
}

// A database opened by this build carries the barrier at sequence 0 under
// the newest migration marker.
func TestFreshDatabaseHasTheCatalogGCBarrier(t *testing.T) {
	st := catalogReferenceFixture(t)
	if got := catalogGCSequence(t, st); got != 0 {
		t.Fatalf("fresh barrier sequence = %d, want 0", got)
	}
	var marker SchemaMigration
	if err := st.DB().Where("version = ?", 245).First(&marker).Error; err != nil || marker.Name != "catalog_gc_barrier" {
		t.Fatalf("marker = %+v err = %v, want version 245 catalog_gc_barrier", marker, err)
	}
	if LatestMigration() < 245 {
		t.Fatalf("LatestMigration() = %d, want at least 245", LatestMigration())
	}
}

// A failure in the middle of the migration, after the table exists, leaves
// neither the table nor the marker.
func TestCatalogGCBarrierMigrationRollsBackAMidwayFailure(t *testing.T) {
	db, err := gorm.Open(sqlite.Open(filepath.Join(t.TempDir(), "midway.db")), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := db.DB()
	if err != nil {
		t.Fatal(err)
	}
	defer pool.Close()
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected failure after the table was created")
	broken := migration{Version: 245, Name: "catalog_gc_barrier", Apply: func(tx *gorm.DB) error {
		if err := migrateCatalogGCBarrier(tx); err != nil {
			return err
		}
		return injected
	}}
	if err := applyMigration(db, broken); !errors.Is(err, injected) {
		t.Fatalf("applyMigration = %v, want the injected failure", err)
	}
	if db.Migrator().HasTable("catalog_gc_barriers") {
		t.Fatal("the table outlived a failed migration")
	}
	var count int64
	if err := db.Model(&SchemaMigration{}).Where("version = 245").Count(&count).Error; err != nil || count != 0 {
		t.Fatalf("marker count = %d err = %v, want none", count, err)
	}
}
