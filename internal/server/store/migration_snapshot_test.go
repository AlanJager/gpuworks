package store

import (
	"regexp"
	"testing"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

// timestampLiteral matches the time literals of a dumped row: migration
// markers and seeded rows are stamped when the chain runs, the only values
// that legitimately differ between two runs of it.
var timestampLiteral = regexp.MustCompile(`'\d{4}-\d\d-\d\d \d\d:\d\d:\d\d[^']*'`)

func normalizedDump(t *testing.T, db *gorm.DB) []string {
	t.Helper()
	stmts, err := dumpDatabase(db)
	if err != nil {
		t.Fatalf("dump: %v", err)
	}
	out := make([]string, len(stmts))
	for i, s := range stmts {
		out[i] = timestampLiteral.ReplaceAllString(s, "<time>")
	}
	return out
}

func openRawMemory(t *testing.T, name string) *gorm.DB {
	t.Helper()
	gdb, err := gorm.Open(sqlite.Open("file:"+name+"?mode=memory&cache=shared"), &gorm.Config{Logger: logger.Default.LogMode(logger.Silent)})
	if err != nil {
		t.Fatal(err)
	}
	pool, err := gdb.DB()
	if err != nil {
		t.Fatal(err)
	}
	pool.SetMaxOpenConns(1)
	t.Cleanup(func() { _ = pool.Close() })
	return gdb
}

func TestAnEmptyInMemoryStoreMatchesTheRealMigrationChain(t *testing.T) {
	real := openRawMemory(t, "snapshot_real_chain")
	if err := migrateDB(real, migrations); err != nil {
		t.Fatalf("real migration chain: %v", err)
	}
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })

	want, got := normalizedDump(t, real), normalizedDump(t, s.db)
	if len(want) < len(migrations) {
		t.Fatalf("real dump has %d statements, want at least one marker per migration (%d)", len(want), len(migrations))
	}
	// Open seeds rows after migrating (terminal settings, deployment policy);
	// those come on top of the migrated state and are not part of the record.
	gotSet := map[string]bool{}
	for _, s := range got {
		gotSet[s] = true
	}
	for _, s := range want {
		if !gotSet[s] {
			t.Errorf("replayed store lacks %s", s)
		}
	}
	if v, err := s.CurrentMigration(); err != nil || v != migrations[len(migrations)-1].Version {
		t.Fatalf("CurrentMigration = %d, %v; want %d", v, err, migrations[len(migrations)-1].Version)
	}
}

func TestReplayedSchemaKeepsAutoincrementAndUniqueness(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	var schemaSQL []string
	if err := s.db.Raw(`SELECT sql FROM sqlite_master WHERE type = 'index' AND sql IS NOT NULL`).Scan(&schemaSQL).Error; err != nil {
		t.Fatal(err)
	}
	if len(schemaSQL) == 0 {
		t.Fatal("replayed store has no indexes")
	}
	if _, err := s.RegisterNode("uuid-a", HashToken("a"), "host-a"); err != nil {
		t.Fatal(err)
	}
	if _, err := s.RegisterNode("uuid-a", HashToken("b"), "host-b"); err == nil {
		t.Fatal("a second node with the same UUID was accepted; unique index missing after replay")
	}
}

func TestAnInMemoryStoreThatAlreadyHasTablesRunsTheRealChain(t *testing.T) {
	// Make sure the process-wide record exists before this database is
	// opened, so markers stamped later can only come from a real run.
	if _, err := migratedSchemaStatements(); err != nil {
		t.Fatal(err)
	}
	keep := openRawMemory(t, "snapshot_nonempty")
	// A database left at an early version: only the first migration applied.
	if err := migrateDB(keep, migrations[:1]); err != nil {
		t.Fatal(err)
	}
	before := time.Now().UTC()
	s, err := Open("file:snapshot_nonempty?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	var markers []SchemaMigration
	if err := s.db.Order("version").Find(&markers).Error; err != nil {
		t.Fatal(err)
	}
	if len(markers) != len(migrations) {
		t.Fatalf("markers = %d, want %d", len(markers), len(migrations))
	}
	for _, m := range markers[1:] {
		if m.AppliedAt.Before(before) {
			t.Fatalf("migration %d applied at %v, before Open started at %v: it came from the record, not the real chain", m.Version, m.AppliedAt, before)
		}
	}
}
