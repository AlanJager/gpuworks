package store

import (
	"errors"
	"fmt"
	"log/slog"
	"regexp"
	"slices"
	"strings"
	"time"

	"gorm.io/gorm"
)

// Migration is one step of an extension's schema. Apply runs inside the
// transaction that also records the step, so a failed step leaves neither
// its schema change nor its marker behind.
type Migration struct {
	Version int
	Name    string
	Apply   func(tx *gorm.DB) error
}

// MigrationSet is the schema of one extension a separately built server
// adds. Its steps are recorded in namespace_migrations under Namespace,
// apart from the core schema_migrations, so the two numberings never meet.
//
// Every table, index, view and trigger a step creates, changes or drops
// must be named with TablePrefix, and an index or trigger must also sit on
// such a table: a step that touches anything else is refused and rolled
// back. Rows of core tables are not checked; an extension writes them only
// through Store's methods. A core sweep over every table (such as the UTC
// time normalisation) may also reach extension tables, so extensions store
// times in UTC and keep their steps additive.
type MigrationSet struct {
	// Namespace names the set: [a-z][a-z0-9_]{1,31}, one per module.
	Namespace string
	// TablePrefix starts the name of every schema object of the set:
	// [a-z][a-z0-9_]* ending in "_", such as "ee_quota_". Core tables
	// never start with ReservedExtensionTablePrefix, so a prefix beginning
	// with it cannot meet a core table, now or later.
	TablePrefix string
	// Migrations run in ascending Version order; versions start at 1 and
	// never repeat, and a step's Name never changes once released.
	Migrations []Migration
}

// ReservedExtensionTablePrefix is kept free of core tables (a test holds the
// core schema to it), so extension sets can prefix their tables with it.
const ReservedExtensionTablePrefix = "ee_"

// NamespaceMigration records an applied step of an extension MigrationSet.
// The table exists only in a database some extension has been opened with.
type NamespaceMigration struct {
	Namespace string `gorm:"primaryKey;size:32"`
	Version   int    `gorm:"primaryKey;autoIncrement:false"`
	Name      string `gorm:"size:128"`
	AppliedAt time.Time
}

var (
	// ErrNamespaceTooNew is returned when the database holds a step of a
	// namespace this binary includes that is newer than any step the binary
	// knows: it was opened by a newer build, and an older one must not run.
	ErrNamespaceTooNew = errors.New("database has a newer migration in this namespace than this binary knows")
	// ErrOutsideTablePrefix is returned when an extension step touches a
	// schema object outside its TablePrefix; the step is rolled back.
	ErrOutsideTablePrefix = errors.New("extension migration touched a schema object outside its table prefix")
)

var (
	namespacePattern   = regexp.MustCompile(`^[a-z][a-z0-9_]{1,31}$`)
	tablePrefixPattern = regexp.MustCompile(`^[a-z][a-z0-9_]{0,30}_$`)
)

// validate reports why set cannot run.
func (set MigrationSet) validate() error {
	if !namespacePattern.MatchString(set.Namespace) {
		return fmt.Errorf("migration namespace %q must match %s", set.Namespace, namespacePattern)
	}
	if !tablePrefixPattern.MatchString(set.TablePrefix) || strings.HasPrefix(set.TablePrefix, "sqlite_") {
		return fmt.Errorf("namespace %s: table prefix %q must match %s and not start with sqlite_", set.Namespace, set.TablePrefix, tablePrefixPattern)
	}
	last := 0
	for _, m := range set.Migrations {
		switch {
		case m.Version <= last:
			return fmt.Errorf("namespace %s: migration %d follows %d; versions start at 1 and ascend", set.Namespace, m.Version, last)
		case strings.TrimSpace(m.Name) == "" || len(m.Name) > 128:
			return fmt.Errorf("namespace %s: migration %d needs a name of at most 128 bytes", set.Namespace, m.Version)
		case m.Apply == nil:
			return fmt.Errorf("namespace %s: migration %d (%s) has no Apply", set.Namespace, m.Version, m.Name)
		}
		last = m.Version
	}
	return nil
}

// validateMigrationSets checks every set and that no two share a namespace
// or a prefix one of the other extends, so each object has one owner.
func validateMigrationSets(sets []MigrationSet) error {
	for i, set := range sets {
		if err := set.validate(); err != nil {
			return err
		}
		for _, other := range sets[:i] {
			if other.Namespace == set.Namespace {
				return fmt.Errorf("migration namespace %s is given twice", set.Namespace)
			}
			if strings.HasPrefix(set.TablePrefix, other.TablePrefix) || strings.HasPrefix(other.TablePrefix, set.TablePrefix) {
				return fmt.Errorf("namespaces %s and %s have overlapping table prefixes %q and %q", other.Namespace, set.Namespace, other.TablePrefix, set.TablePrefix)
			}
		}
	}
	return nil
}

// migrateNamespaces runs every set after the core schema is current. With
// no sets and no namespace_migrations table it does nothing, so a community
// database never gains the table. Rows of namespaces no set names are left
// alone with a warning: a database moved between builds keeps the tables
// of modules this build lacks, and a later build with them carries on.
func migrateNamespaces(db *gorm.DB, sets []MigrationSet) error {
	if len(sets) == 0 && !db.Migrator().HasTable(&NamespaceMigration{}) {
		return nil
	}
	if err := db.AutoMigrate(&NamespaceMigration{}); err != nil {
		return err
	}
	var recorded []string
	if err := db.Model(&NamespaceMigration{}).Distinct("namespace").Order("namespace").Pluck("namespace", &recorded).Error; err != nil {
		return err
	}
	for _, ns := range recorded {
		if !slices.ContainsFunc(sets, func(s MigrationSet) bool { return s.Namespace == ns }) {
			slog.Warn("database holds the tables of an extension this server does not include; they are left as they are", "namespace", ns)
		}
	}
	for _, set := range sets {
		if err := migrateNamespace(db, set); err != nil {
			return fmt.Errorf("namespace %s: %w", set.Namespace, err)
		}
	}
	return nil
}

// migrateNamespace applies the steps of set the database has not recorded.
// Unlike the core chain, a recorded step under another name is a hard error:
// a namespace is young enough never to have renamed a released step.
func migrateNamespace(db *gorm.DB, set MigrationSet) error {
	var rows []NamespaceMigration
	if err := db.Where("namespace = ?", set.Namespace).Order("version").Find(&rows).Error; err != nil {
		return err
	}
	known := make(map[int]string, len(set.Migrations))
	highest := 0
	for _, m := range set.Migrations {
		known[m.Version] = m.Name
		highest = m.Version
	}
	applied := make(map[int]bool, len(rows))
	for _, row := range rows {
		name, ok := known[row.Version]
		switch {
		case !ok && row.Version > highest:
			return fmt.Errorf("%w: migration %d (%s) recorded, this binary knows up to %d", ErrNamespaceTooNew, row.Version, row.Name, highest)
		case !ok:
			return fmt.Errorf("migration %d (%s) is recorded but this binary does not have it", row.Version, row.Name)
		case name != row.Name:
			return fmt.Errorf("migration %d is recorded as %q, this binary names it %q", row.Version, row.Name, name)
		}
		applied[row.Version] = true
	}
	if len(rows) == 0 {
		// The first step of a namespace must find its prefix unused, or the
		// prefix already names objects someone else owns.
		taken, err := objectsWithPrefix(db, set.TablePrefix)
		if err != nil {
			return err
		}
		if len(taken) > 0 {
			return fmt.Errorf("%w: %q already names %s before this namespace's first migration", ErrOutsideTablePrefix, set.TablePrefix, strings.Join(taken, ", "))
		}
	}
	for _, m := range set.Migrations {
		if applied[m.Version] {
			continue
		}
		if err := applyNamespaceMigration(db, set, m); err != nil {
			return fmt.Errorf("migration %d (%s): %w", m.Version, m.Name, err)
		}
	}
	return nil
}

// applyNamespaceMigration runs one step, checks what it did to the schema
// and records it, all in one transaction.
func applyNamespaceMigration(db *gorm.DB, set MigrationSet, m Migration) error {
	return db.Transaction(func(tx *gorm.DB) error {
		before, err := readSchemaObjects(tx)
		if err != nil {
			return err
		}
		if err := m.Apply(tx); err != nil {
			return err
		}
		after, err := readSchemaObjects(tx)
		if err != nil {
			return err
		}
		if outside := objectsOutsidePrefix(set.TablePrefix, before, after); len(outside) > 0 {
			return fmt.Errorf("%w %q: %s", ErrOutsideTablePrefix, set.TablePrefix, strings.Join(outside, ", "))
		}
		return tx.Create(&NamespaceMigration{Namespace: set.Namespace, Version: m.Version, Name: m.Name, AppliedAt: time.Now().UTC()}).Error
	})
}

// schemaEntry is one row of sqlite_master.
type schemaEntry struct {
	Type    string
	Name    string
	TblName string
	SQL     *string
}

func (e schemaEntry) key() string { return e.Type + " " + e.Name }

func readSchemaObjects(db *gorm.DB) (map[string]schemaEntry, error) {
	var rows []schemaEntry
	if err := db.Raw(`SELECT type, name, tbl_name, sql FROM sqlite_master`).Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("read schema: %w", err)
	}
	out := make(map[string]schemaEntry, len(rows))
	for _, r := range rows {
		out[r.key()] = r
	}
	return out, nil
}

// ownedBy reports whether e belongs to the prefix: its name and the table it
// sits on both carry it. SQLite's own objects count by their table alone
// (an automatic index of a prefixed table), or not at all (sqlite_sequence).
func (e schemaEntry) ownedBy(prefix string) bool {
	if strings.HasPrefix(e.Name, "sqlite_") {
		return e.Type != "index" || strings.HasPrefix(e.TblName, prefix)
	}
	return strings.HasPrefix(e.Name, prefix) && strings.HasPrefix(e.TblName, prefix)
}

// objectsOutsidePrefix lists the objects a step created, dropped or changed
// that the prefix does not own.
func objectsOutsidePrefix(prefix string, before, after map[string]schemaEntry) []string {
	var out []string
	sqlOf := func(e schemaEntry) string {
		if e.SQL == nil {
			return ""
		}
		return *e.SQL
	}
	for k, a := range after {
		b, existed := before[k]
		if (!existed || sqlOf(b) != sqlOf(a) || b.TblName != a.TblName) && !a.ownedBy(prefix) {
			out = append(out, k)
		}
	}
	for k, b := range before {
		if _, kept := after[k]; !kept && !b.ownedBy(prefix) {
			out = append(out, k+" (dropped)")
		}
	}
	slices.Sort(out)
	return out
}

// objectsWithPrefix lists the schema objects whose name or table starts with
// prefix.
func objectsWithPrefix(db *gorm.DB, prefix string) ([]string, error) {
	objects, err := readSchemaObjects(db)
	if err != nil {
		return nil, err
	}
	var out []string
	for k, e := range objects {
		if strings.HasPrefix(e.Name, prefix) || (e.Type != "table" && strings.HasPrefix(e.TblName, prefix)) {
			out = append(out, k)
		}
	}
	slices.Sort(out)
	return out, nil
}

// NamespaceVersion is the highest step of namespace the database records,
// 0 when it records none.
func (s *Store) NamespaceVersion(namespace string) (int, error) {
	if !s.db.Migrator().HasTable(&NamespaceMigration{}) {
		return 0, nil
	}
	var version *int
	if err := s.db.Model(&NamespaceMigration{}).Where("namespace = ?", namespace).Select("MAX(version)").Scan(&version).Error; err != nil {
		return 0, err
	}
	if version == nil {
		return 0, nil
	}
	return *version, nil
}
