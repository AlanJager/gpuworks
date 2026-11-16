package store

import (
	"bytes"
	"errors"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

func createStep(version int, name, stmt string) Migration {
	return Migration{Version: version, Name: name, Apply: func(tx *gorm.DB) error { return tx.Exec(stmt).Error }}
}

// quotaSet is a two-step extension schema: a table, then an index on it.
func quotaSet() MigrationSet {
	return MigrationSet{Namespace: "quota", TablePrefix: "ee_quota_", Migrations: []Migration{
		createStep(1, "items", "CREATE TABLE ee_quota_items (id INTEGER PRIMARY KEY AUTOINCREMENT, key TEXT NOT NULL UNIQUE)"),
		createStep(2, "items key index", "CREATE INDEX ee_quota_items_by_key ON ee_quota_items (key)"),
	}}
}

func openWithSets(t *testing.T, path string, sets ...MigrationSet) (*Store, error) {
	t.Helper()
	s, err := OpenWith(path, bus.New(), OpenOptions{Extensions: sets})
	if err == nil {
		t.Cleanup(func() { _ = s.Close() })
	}
	return s, err
}

func mustOpenWithSets(t *testing.T, path string, sets ...MigrationSet) *Store {
	t.Helper()
	s, err := openWithSets(t, path, sets...)
	if err != nil {
		t.Fatalf("open with %d extension sets: %v", len(sets), err)
	}
	return s
}

// reopen closes s and opens its file again with sets.
func reopen(t *testing.T, s *Store, path string, sets ...MigrationSet) (*Store, error) {
	t.Helper()
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	return openWithSets(t, path, sets...)
}

func markers(t *testing.T, s *Store, namespace string) []NamespaceMigration {
	t.Helper()
	var rows []NamespaceMigration
	if err := s.db.Where("namespace = ?", namespace).Order("version").Find(&rows).Error; err != nil {
		t.Fatal(err)
	}
	return rows
}

// captureWarnings sends slog's default logger to a buffer for the test.
func captureWarnings(t *testing.T) *bytes.Buffer {
	t.Helper()
	var buf bytes.Buffer
	saved := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&buf, &slog.HandlerOptions{Level: slog.LevelWarn})))
	t.Cleanup(func() { slog.SetDefault(saved) })
	return &buf
}

func TestNamespaceMigrationsCreateTheSchemaAndRecordEachStep(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gpuworks.db")
	s := mustOpenWithSets(t, path, quotaSet())
	if !s.db.Migrator().HasTable("ee_quota_items") || !s.db.Migrator().HasIndex("ee_quota_items", "ee_quota_items_by_key") {
		t.Fatal("the extension's table or index is missing")
	}
	rows := markers(t, s, "quota")
	if len(rows) != 2 || rows[0].Version != 1 || rows[0].Name != "items" || rows[1].Version != 2 || rows[1].Name != "items key index" || rows[1].AppliedAt.IsZero() {
		t.Fatalf("markers = %+v", rows)
	}
	if v, err := s.NamespaceVersion("quota"); err != nil || v != 2 {
		t.Fatalf("NamespaceVersion = %d, %v; want 2", v, err)
	}
	if v, err := s.NamespaceVersion("billing"); err != nil || v != 0 {
		t.Fatalf("NamespaceVersion of an unknown namespace = %d, %v; want 0", v, err)
	}
	// The core numbering is untouched.
	var coreMarkers int64
	if err := s.db.Model(&SchemaMigration{}).Where("name IN ?", []string{"items", "items key index"}).Count(&coreMarkers).Error; err != nil || coreMarkers != 0 {
		t.Fatalf("extension steps reached schema_migrations: %d, %v", coreMarkers, err)
	}
	if core, err := s.CurrentMigration(); err != nil || core != migrations[len(migrations)-1].Version {
		t.Fatalf("CurrentMigration = %d, %v", core, err)
	}
}

func TestNamespaceMigrationsRerunIdempotently(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gpuworks.db")
	applies := map[int]int{}
	set := quotaSet()
	for i := range set.Migrations {
		m := &set.Migrations[i]
		apply, version := m.Apply, m.Version
		m.Apply = func(tx *gorm.DB) error { applies[version]++; return apply(tx) }
	}
	s := mustOpenWithSets(t, path, set)
	s, err := reopen(t, s, path, set)
	if err != nil {
		t.Fatalf("second open: %v", err)
	}
	if applies[1] != 1 || applies[2] != 1 {
		t.Fatalf("applies = %v, want each step once", applies)
	}
	if rows := markers(t, s, "quota"); len(rows) != 2 {
		t.Fatalf("markers after a rerun = %+v", rows)
	}
}

// A step that fails part way leaves neither its schema nor its marker; the
// steps before it stay, and the next start with a working step carries on.
func TestNamespaceMigrationFailureRollsBackSchemaAndMarker(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gpuworks.db")
	injected := errors.New("injected step failure")
	broken := quotaSet()
	broken.Migrations[1] = Migration{Version: 2, Name: "items key index", Apply: func(tx *gorm.DB) error {
		if err := tx.Exec("CREATE TABLE ee_quota_half (id INTEGER PRIMARY KEY)").Error; err != nil {
			return err
		}
		return injected
	}}
	if _, err := openWithSets(t, path, broken); !errors.Is(err, injected) {
		t.Fatalf("open = %v, want the injected failure", err)
	}
	s, err := Open(path, bus.New())
	if err != nil {
		t.Fatalf("a community open after the failure: %v", err)
	}
	if s.db.Migrator().HasTable("ee_quota_half") {
		t.Fatal("the failed step's table survived")
	}
	if !s.db.Migrator().HasTable("ee_quota_items") {
		t.Fatal("the step before the failure was rolled back with it")
	}
	if v, err := s.NamespaceVersion("quota"); err != nil || v != 1 {
		t.Fatalf("NamespaceVersion = %d, %v; want 1", v, err)
	}
	s, err = reopen(t, s, path, quotaSet())
	if err != nil {
		t.Fatalf("open with the working step: %v", err)
	}
	if v, _ := s.NamespaceVersion("quota"); v != 2 || !s.db.Migrator().HasIndex("ee_quota_items", "ee_quota_items_by_key") {
		t.Fatalf("the working step did not run: version %d", v)
	}
}

func TestNamespaceAheadOfTheBinaryRefusesToOpen(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gpuworks.db")
	s := mustOpenWithSets(t, path, quotaSet())
	older := quotaSet()
	older.Migrations = older.Migrations[:1]
	if _, err := reopen(t, s, path, older); !errors.Is(err, ErrNamespaceTooNew) {
		t.Fatalf("open with an older set = %v, want ErrNamespaceTooNew", err)
	}
}

// Core migrations only warn about a renamed step; a namespace refuses it,
// and a recorded step the binary lacks.
func TestNamespaceStepRecordedDifferentlyIsAHardError(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gpuworks.db")
	s := mustOpenWithSets(t, path, quotaSet())
	renamed := quotaSet()
	renamed.Migrations[0].Name = "quota items"
	if _, err := reopen(t, s, path, renamed); err == nil || errors.Is(err, ErrNamespaceTooNew) || !strings.Contains(err.Error(), `recorded as "items"`) {
		t.Fatalf("open with a renamed step = %v", err)
	}
	gap := quotaSet()
	gap.Migrations = []Migration{gap.Migrations[0], createStep(3, "later", "CREATE TABLE ee_quota_later (id INTEGER)")}
	if _, err := openWithSets(t, path, gap); err == nil || errors.Is(err, ErrNamespaceTooNew) || !strings.Contains(err.Error(), "does not have it") {
		t.Fatalf("open with a set lacking a recorded step = %v", err)
	}
}

// A database moved between builds keeps the tables of modules a build lacks:
// they are ignored with a warning, and a build with the module carries on.
func TestUnknownNamespacesAreIgnoredWithAWarning(t *testing.T) {
	path := filepath.Join(t.TempDir(), "gpuworks.db")
	s := mustOpenWithSets(t, path, quotaSet())
	if err := s.db.Exec("INSERT INTO ee_quota_items (key) VALUES ('k1')").Error; err != nil {
		t.Fatal(err)
	}
	warnings := captureWarnings(t)
	billing := MigrationSet{Namespace: "billing", TablePrefix: "ee_billing_", Migrations: []Migration{
		createStep(1, "rates", "CREATE TABLE ee_billing_rates (id INTEGER PRIMARY KEY)"),
	}}
	s, err := reopen(t, s, path, billing)
	if err != nil {
		t.Fatalf("open with another module only: %v", err)
	}
	if !strings.Contains(warnings.String(), "namespace=quota") || strings.Contains(warnings.String(), "namespace=billing") {
		t.Fatalf("warnings = %q, want the quota namespace named", warnings.String())
	}
	warnings.Reset()
	s, err = reopen(t, s, path)
	if err != nil {
		t.Fatalf("community open of an extended database: %v", err)
	}
	if !strings.Contains(warnings.String(), "namespace=quota") || !strings.Contains(warnings.String(), "namespace=billing") {
		t.Fatalf("warnings = %q, want both namespaces named", warnings.String())
	}
	s, err = reopen(t, s, path, quotaSet(), billing)
	if err != nil {
		t.Fatalf("back to the full build: %v", err)
	}
	var keys []string
	if err := s.db.Raw("SELECT key FROM ee_quota_items").Scan(&keys).Error; err != nil || len(keys) != 1 || keys[0] != "k1" {
		t.Fatalf("quota rows after the round trip = %q, %v", keys, err)
	}
	if rows := markers(t, s, "quota"); len(rows) != 2 {
		t.Fatalf("quota markers after the round trip = %+v", rows)
	}
}

func TestNamespaceStepOutsideItsPrefixIsRefused(t *testing.T) {
	cases := map[string]string{
		"table":            "CREATE TABLE other_items (id INTEGER)",
		"index on core":    "CREATE INDEX ee_quota_nodes_by_name ON nodes (name)",
		"column on core":   "ALTER TABLE nodes ADD COLUMN ee_quota_seat INTEGER",
		"dropped core":     "DROP TABLE notice_reads",
		"trigger on core":  "CREATE TRIGGER ee_quota_watch AFTER INSERT ON nodes BEGIN SELECT 1; END",
		"view of another":  "CREATE VIEW quota_view AS SELECT 1",
		"prefixed and not": "CREATE TABLE ee_quota_ok (id INTEGER); CREATE TABLE stray (id INTEGER)",
	}
	for name, stmt := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "gpuworks.db")
			set := MigrationSet{Namespace: "quota", TablePrefix: "ee_quota_", Migrations: []Migration{
				createStep(1, "items", "CREATE TABLE ee_quota_items (id INTEGER PRIMARY KEY)"),
				createStep(2, "stray", stmt),
			}}
			if _, err := openWithSets(t, path, set); !errors.Is(err, ErrOutsideTablePrefix) {
				t.Fatalf("open = %v, want ErrOutsideTablePrefix", err)
			}
			s, err := Open(path, bus.New())
			if err != nil {
				t.Fatal(err)
			}
			defer s.Close()
			if v, _ := s.NamespaceVersion("quota"); v != 1 {
				t.Fatalf("NamespaceVersion = %d, want the refused step unrecorded", v)
			}
			for _, table := range []string{"other_items", "stray", "ee_quota_ok"} {
				if s.db.Migrator().HasTable(table) {
					t.Fatalf("%s survived the refused step", table)
				}
			}
			if !s.db.Migrator().HasTable("notice_reads") || s.db.Migrator().HasColumn("nodes", "ee_quota_seat") {
				t.Fatal("the refused step changed a core table")
			}
		})
	}
}

// A prefix that already names objects before a namespace's first step is
// someone else's; the first step is refused rather than adopting them.
func TestNamespacePrefixAlreadyInUseIsRefused(t *testing.T) {
	set := MigrationSet{Namespace: "nodes_ext", TablePrefix: "node_", Migrations: []Migration{
		createStep(1, "extra", "CREATE TABLE node_extra (id INTEGER)"),
	}}
	if _, err := openWithSets(t, filepath.Join(t.TempDir(), "gpuworks.db"), set); !errors.Is(err, ErrOutsideTablePrefix) || !strings.Contains(err.Error(), "node_") {
		t.Fatalf("open = %v, want the used prefix refused", err)
	}
}

func TestMalformedMigrationSetsAreRefusedBeforeTheDatabaseIsTouched(t *testing.T) {
	ok := createStep(1, "items", "CREATE TABLE ee_quota_items (id INTEGER)")
	cases := map[string][]MigrationSet{
		"namespace":         {{Namespace: "Quota", TablePrefix: "ee_quota_", Migrations: []Migration{ok}}},
		"short namespace":   {{Namespace: "l", TablePrefix: "ee_quota_", Migrations: []Migration{ok}}},
		"prefix":            {{Namespace: "quota", TablePrefix: "ee_quota", Migrations: []Migration{ok}}},
		"sqlite prefix":     {{Namespace: "quota", TablePrefix: "sqlite_x_", Migrations: []Migration{ok}}},
		"version zero":      {{Namespace: "quota", TablePrefix: "ee_quota_", Migrations: []Migration{createStep(0, "zero", "SELECT 1")}}},
		"descending":        {{Namespace: "quota", TablePrefix: "ee_quota_", Migrations: []Migration{createStep(2, "b", "SELECT 1"), createStep(1, "a", "SELECT 1")}}},
		"no name":           {{Namespace: "quota", TablePrefix: "ee_quota_", Migrations: []Migration{createStep(1, " ", "SELECT 1")}}},
		"no apply":          {{Namespace: "quota", TablePrefix: "ee_quota_", Migrations: []Migration{{Version: 1, Name: "a"}}}},
		"twice":             {quotaSet(), quotaSet()},
		"overlapping":       {quotaSet(), {Namespace: "quota_ext", TablePrefix: "ee_quota_ext_", Migrations: []Migration{ok}}},
		"overlapping first": {{Namespace: "all", TablePrefix: "ee_", Migrations: []Migration{ok}}, quotaSet()},
	}
	for name, sets := range cases {
		t.Run(name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "gpuworks.db")
			if _, err := openWithSets(t, path, sets...); err == nil {
				t.Fatal("open accepted a malformed set")
			}
			if _, err := os.Stat(path); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("the database file was touched: %v", err)
			}
		})
	}
}

// A community open never gains the namespace table.
func TestOpenWithoutExtensionsCreatesNoNamespaceTable(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	if s.db.Migrator().HasTable(&NamespaceMigration{}) {
		t.Fatal("namespace_migrations exists without any extension")
	}
	if v, err := s.NamespaceVersion("quota"); err != nil || v != 0 {
		t.Fatalf("NamespaceVersion = %d, %v", v, err)
	}
	if _, err := s.ExtensionTables("quota"); !errors.Is(err, ErrUnknownNamespace) {
		t.Fatalf("ExtensionTables = %v, want ErrUnknownNamespace", err)
	}
}

// An empty in-memory open replays the recorded core schema; extension sets
// run after the replay on that database and never enter the record.
func TestInMemorySnapshotStaysCoreOnly(t *testing.T) {
	// Not newTestStore's name: the community store below must be a database
	// of its own.
	s := mustOpenWithSets(t, "file:"+t.Name()+"-extended?mode=memory&cache=shared", quotaSet())
	if !s.db.Migrator().HasTable("ee_quota_items") || !s.db.Migrator().HasTable(&NamespaceMigration{}) {
		t.Fatal("the extension did not run on the in-memory database")
	}
	stmts, err := migratedSchemaStatements()
	if err != nil {
		t.Fatal(err)
	}
	for _, stmt := range stmts {
		if strings.Contains(stmt, "namespace_migrations") || strings.Contains(stmt, "ee_quota_") {
			t.Fatalf("the core schema record holds extension schema: %s", stmt)
		}
	}
	plain := newTestStore(t)
	t.Cleanup(func() { _ = plain.Close() })
	if plain.db.Migrator().HasTable("ee_quota_items") || plain.db.Migrator().HasTable(&NamespaceMigration{}) {
		t.Fatal("a later community in-memory open inherited the extension schema")
	}
}

// Core tables keep clear of the reserved extension prefix, so an extension
// whose prefix starts with it can never meet a core table, now or after a
// later core migration (this test then fails).
func TestNoCoreSchemaObjectUsesTheReservedExtensionPrefix(t *testing.T) {
	s := newTestStore(t)
	t.Cleanup(func() { _ = s.Close() })
	objects, err := readSchemaObjects(s.db)
	if err != nil {
		t.Fatal(err)
	}
	if len(objects) < len(migrations)/4 {
		t.Fatalf("read only %d schema objects", len(objects))
	}
	for key, o := range objects {
		if strings.HasPrefix(o.Name, ReservedExtensionTablePrefix) || strings.HasPrefix(o.TblName, ReservedExtensionTablePrefix) {
			t.Errorf("core schema object %s uses the reserved extension prefix %q", key, ReservedExtensionTablePrefix)
		}
	}
}
