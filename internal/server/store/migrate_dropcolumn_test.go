package store

import (
	"bytes"
	"os"
	"slices"
	"testing"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// Dropping a column without losing the table's indexes. gorm's own
// Migrator reports success, keeps the column and destroys every index on
// the table, so store.dropColumn does it with native SQL and these tests
// hold both halves of that.

// Dropping a column must keep the table's indexes, and must actually drop
// the column. gorm's own migrator does neither on this driver: measured
// against nodes it returned no error, left the column in place, and took
// all seven named indexes with it. store.dropColumn uses SQLite's real
// DROP COLUMN instead and checks the indexes survived.
func TestDropColumnRemovesTheColumnAndKeepsTheIndexes(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:drop-column-indexes?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Node{}); err != nil {
		t.Fatal(err)
	}
	indexes := func() map[string]string {
		defs, err := indexDefinitions(db, "nodes")
		if err != nil {
			t.Fatal(err)
		}
		return defs
	}
	before := indexes()
	if len(before) < 2 {
		t.Fatalf("the fixture has %d named indexes; it cannot show any being lost", len(before))
	}
	if err := db.Exec("ALTER TABLE nodes ADD COLUMN scratch varchar(8)").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("INSERT INTO nodes (uuid, name, state, scratch) VALUES ('keep','keep','online','x')").Error; err != nil {
		t.Fatal(err)
	}

	if err := dropColumn(db, "nodes", "scratch"); err != nil {
		t.Fatalf("dropColumn: %v", err)
	}
	var columns []struct {
		Name string `gorm:"column:name"`
	}
	if err := db.Raw("SELECT name FROM pragma_table_info('nodes')").Scan(&columns).Error; err != nil {
		t.Fatal(err)
	}
	for _, column := range columns {
		if column.Name == "scratch" {
			t.Fatal("the column is still there")
		}
	}
	after := indexes()
	if len(after) != len(before) {
		t.Fatalf("kept %d of %d named indexes", len(after), len(before))
	}
	for name, def := range before {
		if after[name] != def {
			t.Fatalf("index %s came back as %q, want %q", name, after[name], def)
		}
	}
	// The rows are still there, and so is the data in the other columns.
	var kept int64
	if err := db.Raw("SELECT COUNT(*) FROM nodes WHERE uuid = 'keep' AND state = 'online'").Scan(&kept).Error; err != nil {
		t.Fatal(err)
	}
	if kept != 1 {
		t.Fatalf("rows with their other columns intact = %d, want 1", kept)
	}
}

// A column an index still names cannot be dropped silently: SQLite refuses,
// so a migration has to drop that index first rather than find out later.
func TestDropColumnRefusesWhileAnIndexNamesIt(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:drop-column-indexed?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&Node{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("ALTER TABLE nodes ADD COLUMN scratch varchar(8)").Error; err != nil {
		t.Fatal(err)
	}
	if err := db.Exec("CREATE INDEX idx_nodes_scratch ON nodes(scratch)").Error; err != nil {
		t.Fatal(err)
	}
	if err := dropColumn(db, "nodes", "scratch"); err == nil {
		t.Fatal("dropping an indexed column was allowed")
	}
}

// Migration 105 drops two columns from network_test_runs. It used gorm's
// migrator and so did neither: the columns stayed and the table's index
// went. This pins the fixed behavior for the one other migration in the
// repository that drops a column.
func TestMigration105DropsItsColumnsAndKeepsTheIndex(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:migration-105-columns?mode=memory&cache=shared"), &gorm.Config{})
	if err != nil {
		t.Fatal(err)
	}
	if err := db.AutoMigrate(&SchemaMigration{}, &NetworkTestRun{}); err != nil {
		t.Fatal(err)
	}
	for _, stmt := range []string{
		"ALTER TABLE network_test_runs ADD COLUMN image varchar(512)",
		"ALTER TABLE network_test_runs ADD COLUMN pull_image numeric",
		"INSERT INTO network_test_runs (network_id, status, image) VALUES (7, 'done', 'old/image:tag')",
	} {
		if err := db.Exec(stmt).Error; err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	before, err := indexDefinitions(db, "network_test_runs")
	if err != nil {
		t.Fatal(err)
	}
	if len(before) == 0 {
		t.Fatal("the fixture has no index to lose")
	}

	if err := migrateDB(db, []migration{migrationByVersion(t, 105)}); err != nil {
		t.Fatalf("migration: %v", err)
	}

	var columns []struct {
		Name string `gorm:"column:name"`
	}
	if err := db.Raw("SELECT name FROM pragma_table_info('network_test_runs')").Scan(&columns).Error; err != nil {
		t.Fatal(err)
	}
	for _, column := range columns {
		if column.Name == "image" || column.Name == "pull_image" {
			t.Fatalf("column %s survived the migration", column.Name)
		}
	}
	after, err := indexDefinitions(db, "network_test_runs")
	if err != nil {
		t.Fatal(err)
	}
	if len(after) != len(before) {
		t.Fatalf("kept %d of %d indexes", len(after), len(before))
	}
	var kept int64
	if err := db.Raw("SELECT COUNT(*) FROM network_test_runs WHERE network_id = 7").Scan(&kept).Error; err != nil {
		t.Fatal(err)
	}
	if kept != 1 {
		t.Fatalf("rows after the drop = %d, want the one still there", kept)
	}
}

// No migration may reach for gorm's migrator to drop a column: on this
// driver it keeps the column and destroys the table's indexes, and says
// nothing went wrong.
func TestNoMigrationUsesTheMigratorToDropAColumn(t *testing.T) {
	source, err := os.ReadFile("migrations.go")
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(source, []byte("Migrator().DropColumn")) {
		t.Fatal("migrations.go calls Migrator().DropColumn; use store.dropColumn, which keeps the indexes and checks")
	}
}

// A database that stored a card's verdict beside its facts — the
// schedulable column with its index, and the foreign-process count — loses
// both on upgrade and keeps every other index. A fresh one never has them.
func TestAnUpgradedCardTableLosesTheStoredVerdict(t *testing.T) {
	indexes := func(db *gorm.DB) []string {
		t.Helper()
		var names []string
		if err := db.Raw("SELECT name FROM sqlite_master WHERE type='index' AND tbl_name='gpus' AND name NOT LIKE 'sqlite_%' ORDER BY name").Scan(&names).Error; err != nil {
			t.Fatal(err)
		}
		return names
	}
	fresh := newTestStore(t)
	for _, column := range []string{"schedulable", "foreign_process_count"} {
		if present, err := tableHasColumn(fresh.db, "gpus", column); err != nil || present {
			t.Fatalf("a fresh gpus table has %s (err %v)", column, err)
		}
	}
	if slices.Contains(indexes(fresh.db), "idx_gpus_schedulable_for_sql") {
		t.Fatal("a fresh gpus table indexes a verdict it does not store")
	}

	upgraded, err := gorm.Open(sqlite.Open("file:"+t.Name()+"-upgraded?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if sql, err := upgraded.DB(); err == nil {
			_ = sql.Close()
		}
	})
	if err := upgraded.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	// The table as the branch builds that stored the verdict left it.
	for _, stmt := range []string{
		`CREATE TABLE gpus (id INTEGER PRIMARY KEY, node_id INTEGER, "index" INTEGER, ident TEXT, uuid TEXT, name TEXT, vendor TEXT, memory_total INTEGER, ownership TEXT, health_status TEXT, health_code TEXT, health_message TEXT, healthy NUMERIC, schedulable NUMERIC, foreign_process_count INTEGER, foreign_processes TEXT)`,
		// Later storage migrations run in this upgrade too. Keep their
		// prerequisite table in its historical shape, without override_only.
		`CREATE TABLE storage_nodes (id INTEGER PRIMARY KEY, storage_id INTEGER, node_id INTEGER, label_key TEXT, label_value TEXT, path TEXT, cache_path TEXT, cache_max_bytes INTEGER, existing NUMERIC, created_at DATETIME)`,
		"CREATE UNIQUE INDEX idx_node_card ON gpus(node_id, ident)",
		"CREATE INDEX idx_gpus_vendor ON gpus(vendor)",
		"CREATE INDEX idx_gpus_schedulable_for_sql ON gpus(schedulable)",
	} {
		if err := upgraded.Exec(stmt).Error; err != nil {
			t.Fatal(err)
		}
	}
	var from []migration
	for _, m := range migrations {
		if m.Version >= migrationByName(t, "card_schedulable_verdict").Version {
			from = append(from, m)
		}
	}
	if err := migrateDB(upgraded, from); err != nil {
		t.Fatal(err)
	}
	for _, column := range []string{"schedulable", "foreign_process_count", "healthy"} {
		if present, err := tableHasColumn(upgraded, "gpus", column); err != nil || present {
			t.Fatalf("an upgraded gpus table still has %s (err %v)", column, err)
		}
	}
	if got := indexes(upgraded); !slices.Equal(got, []string{"idx_gpus_vendor", "idx_node_card"}) {
		t.Fatalf("indexes after the upgrade = %v, want the verdict's gone and the rest kept", got)
	}
	// Running it again finds nothing to do.
	if err := migrateDB(upgraded, from); err != nil {
		t.Fatal(err)
	}
}

// A database that ran a different migration under a number — a branch
// build, typically — is warned about and not failed: master has renamed
// migrations it had already shipped, and refusing to start would strand
// those installations. The recorded one stands as applied.
func TestAMigrationRecordedUnderAnotherNameIsSkippedNotFailed(t *testing.T) {
	db, err := gorm.Open(sqlite.Open("file:"+t.Name()+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Discard})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if sql, err := db.DB(); err == nil {
			_ = sql.Close()
		}
	})
	if err := db.AutoMigrate(&SchemaMigration{}); err != nil {
		t.Fatal(err)
	}
	if err := db.Create(&SchemaMigration{Version: 7, Name: "something_else"}).Error; err != nil {
		t.Fatal(err)
	}
	ran := false
	if err := migrateDB(db, []migration{{Version: 7, Name: "what_this_build_calls_it", Apply: func(*gorm.DB) error {
		ran = true
		return nil
	}}}); err != nil {
		t.Fatalf("a renamed migration stopped the upgrade: %v", err)
	}
	if ran {
		t.Fatal("a migration recorded as applied ran again")
	}
}
