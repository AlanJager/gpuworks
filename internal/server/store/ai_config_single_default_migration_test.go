package store

import (
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
)

// openBeforeSingleDefault is a database written before the one-default
// invariant: the ai_configs table as an older build declared it, and rows in the
// state that build could leave behind.
func openBeforeSingleDefault(t *testing.T, name string, rows string) *gorm.DB {
	t.Helper()
	db, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		`CREATE TABLE ai_configs (id INTEGER PRIMARY KEY AUTOINCREMENT, name TEXT, preset TEXT, base_url TEXT, api_key TEXT, model TEXT, is_default NUMERIC, created_at DATETIME, updated_at DATETIME)`,
		rows,
	} {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatal(err)
		}
	}
	return db
}

// singleDefaultRows answers which row is the default, failing on none or many.
func singleDefaultRows(t *testing.T, db *gorm.DB) uint {
	t.Helper()
	var rows []AIConfig
	if err := db.Order("id desc").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	var marked []uint
	for _, row := range rows {
		if row.IsDefault {
			marked = append(marked, row.ID)
		}
	}
	if len(marked) != 1 {
		t.Fatalf("defaults=%v rows=%+v", marked, rows)
	}
	return marked[0]
}

// The migration repairs a database that a build without the invariant left with
// no marked default at all: deleting the default used to leave every stored
// service unmarked, and the read path then fell back to the OLDEST row while the
// console listed the NEWEST first.
func TestAIConfigSingleDefaultMigrationRepairsZeroDefaults(t *testing.T) {
	db := openBeforeSingleDefault(t, "ai-default-none",
		`INSERT INTO ai_configs (id, name, is_default) VALUES (1, 'oldest', 0), (2, 'middle', 0), (3, 'newest', 0)`)
	if err := migrateDB(db, []migration{migrationByName(t, "ai_config_single_default")}); err != nil {
		t.Fatal(err)
	}
	// The newest keeps the role, because that is the row the console lists
	// first and the row a delete promotes: after the upgrade the operator sees
	// the service the assistant actually uses.
	if got := singleDefaultRows(t, db); got != 3 {
		t.Fatalf("default = %d, want the newest row 3", got)
	}
	var marker SchemaMigration
	if err := db.Where("version = ?", migrationByName(t, "ai_config_single_default").Version).First(&marker).Error; err != nil {
		t.Fatalf("marker missing: %v", err)
	}
	// The index now refuses the state the old build could write.
	if err := db.Exec("UPDATE ai_configs SET is_default = 1 WHERE id = 1").Error; err == nil {
		t.Fatal("a second default was accepted after the migration")
	}
}

// The same migration repairs a database with several marked rows, which the
// old read-then-write update could leave behind when two writes interleaved.
func TestAIConfigSingleDefaultMigrationRepairsSeveralDefaults(t *testing.T) {
	db := openBeforeSingleDefault(t, "ai-default-many",
		`INSERT INTO ai_configs (id, name, is_default) VALUES (1, 'oldest', 1), (2, 'middle', 1), (3, 'newest', 1)`)
	if err := migrateDB(db, []migration{migrationByName(t, "ai_config_single_default")}); err != nil {
		t.Fatal(err)
	}
	if got := singleDefaultRows(t, db); got != 3 {
		t.Fatalf("default = %d, want the newest row 3", got)
	}
	// Every other row is still readable: the repair clears marks, it does not
	// delete services.
	var rows []AIConfig
	if err := db.Order("id").Find(&rows).Error; err != nil || len(rows) != 3 {
		t.Fatalf("rows=%d err=%v", len(rows), err)
	}
}

// A database that already holds the invariant is left as it is: the migration
// must not move the default of a healthy installation on upgrade.
func TestAIConfigSingleDefaultMigrationKeepsAHealthyDefault(t *testing.T) {
	db := openBeforeSingleDefault(t, "ai-default-healthy",
		`INSERT INTO ai_configs (id, name, is_default) VALUES (1, 'chosen', 1), (2, 'newer', 0)`)
	if err := migrateDB(db, []migration{migrationByName(t, "ai_config_single_default")}); err != nil {
		t.Fatal(err)
	}
	if got := singleDefaultRows(t, db); got != 1 {
		t.Fatalf("default = %d, want the stored choice 1", got)
	}
}

// An empty database is the normal case for a fresh install: the migration still
// creates the index, so the first service written through any path cannot
// become a second default later.
func TestAIConfigSingleDefaultMigrationIndexesAnEmptyTable(t *testing.T) {
	db := openBeforeSingleDefault(t, "ai-default-empty", `SELECT 1`)
	if err := migrateDB(db, []migration{migrationByName(t, "ai_config_single_default")}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO ai_configs (id, name, is_default) VALUES (1, 'first', 1)`).Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec(`INSERT INTO ai_configs (id, name, is_default) VALUES (2, 'second', 1)`).Error; err == nil {
		t.Fatal("a second default was accepted on a database created after the migration")
	}
	// Several unmarked rows are the ordinary state and must stay allowed: the
	// index is partial for exactly this reason.
	if err := db.Exec(`INSERT INTO ai_configs (id, name, is_default) VALUES (3, 'third', 0), (4, 'fourth', 0)`).Error; err != nil {
		t.Fatalf("unmarked rows rejected: %v", err)
	}
}

// A database with no ai_configs table at all - a fixture that only creates what
// it exercises - is left alone rather than failing startup.
func TestAIConfigSingleDefaultMigrationSkipsADatabaseWithoutTheTable(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:ai-default-no-table?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := migrateDB(db, []migration{migrationByName(t, "ai_config_single_default")}); err != nil {
		t.Fatal(err)
	}
}
