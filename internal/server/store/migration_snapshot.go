package store

import (
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/glebarez/sqlite"
	"gorm.io/gorm"
	"gorm.io/gorm/logger"
)

// An in-memory database is always opened empty, and replaying the whole
// migration chain on it costs a few hundred milliseconds — paid by every test
// that opens a store, hundreds of times per package. The chain is
// deterministic on an empty database, so the first such open in a process
// runs it for real on a private template and records the result as SQL; later
// empty in-memory opens replay that record instead. File databases, and
// in-memory ones that already hold tables, always take the real migration
// path, so upgrade behaviour is never served from the record.
var migratedSchema struct {
	once  sync.Once
	stmts []string
	err   error
}

// migrateInMemory brings an in-memory database up to the current schema.
func (s *Store) migrateInMemory() error {
	empty, err := databaseIsEmpty(s.db)
	if err != nil {
		return err
	}
	if !empty {
		return s.migrate()
	}
	stmts, err := migratedSchemaStatements()
	if err != nil {
		return err
	}
	return s.db.Transaction(func(tx *gorm.DB) error {
		for _, stmt := range stmts {
			if err := tx.Exec(stmt).Error; err != nil {
				return fmt.Errorf("replay migrated schema: %w", err)
			}
		}
		return nil
	})
}

func migratedSchemaStatements() ([]string, error) {
	migratedSchema.once.Do(func() {
		migratedSchema.stmts, migratedSchema.err = recordMigratedSchema(migrations)
	})
	return migratedSchema.stmts, migratedSchema.err
}

// recordMigratedSchema runs migrationList on a fresh private in-memory
// database and returns the statements that recreate the result: every table,
// then its rows (the migration markers and any seeded data, including
// AUTOINCREMENT counters), then indexes, views and triggers.
func recordMigratedSchema(migrationList []migration) ([]string, error) {
	const dsn = "file:gpuworks-migrated-schema?mode=memory&cache=shared"
	gdb, err := gorm.Open(sqlite.Open(dsn), &gorm.Config{
		Logger:  logger.Default.LogMode(logger.Silent),
		NowFunc: func() time.Time { return time.Now().UTC() },
	})
	if err != nil {
		return nil, fmt.Errorf("open migration template: %w", err)
	}
	pool, err := gdb.DB()
	if err != nil {
		return nil, err
	}
	defer pool.Close()
	pool.SetMaxOpenConns(1)
	if err := migrateDB(gdb, migrationList); err != nil {
		return nil, err
	}
	return dumpDatabase(gdb)
}

type schemaObject struct {
	Type string
	Name string
	SQL  string
}

func dumpDatabase(db *gorm.DB) ([]string, error) {
	var objects []schemaObject
	if err := db.Raw(`SELECT type, name, sql FROM sqlite_master
		WHERE sql IS NOT NULL AND name NOT LIKE 'sqlite_%'
		ORDER BY CASE type WHEN 'table' THEN 0 WHEN 'index' THEN 1 WHEN 'view' THEN 2 ELSE 3 END, rowid`).
		Scan(&objects).Error; err != nil {
		return nil, fmt.Errorf("read schema: %w", err)
	}
	var tables, rest []string
	var tableNames []string
	for _, o := range objects {
		if o.Type == "table" {
			tables = append(tables, o.SQL)
			tableNames = append(tableNames, o.Name)
		} else {
			rest = append(rest, o.SQL)
		}
	}
	var hasSequence int64
	if err := db.Raw(`SELECT count(*) FROM sqlite_master WHERE name = 'sqlite_sequence'`).Scan(&hasSequence).Error; err != nil {
		return nil, err
	}
	if hasSequence > 0 {
		tableNames = append(tableNames, "sqlite_sequence")
	}
	stmts := tables
	for _, table := range tableNames {
		rows, err := dumpRows(db, table)
		if err != nil {
			return nil, err
		}
		stmts = append(stmts, rows...)
	}
	return append(stmts, rest...), nil
}

// dumpRows renders every row of table as an INSERT, letting SQLite's quote()
// produce the literals so each value keeps its exact storage class.
func dumpRows(db *gorm.DB, table string) ([]string, error) {
	var columns []string
	if err := db.Raw(`SELECT name FROM pragma_table_info(?) ORDER BY cid`, table).Scan(&columns).Error; err != nil {
		return nil, fmt.Errorf("read columns of %s: %w", table, err)
	}
	if len(columns) == 0 {
		return nil, fmt.Errorf("read columns of %s: none", table)
	}
	quoted := make([]string, len(columns))
	values := make([]string, len(columns))
	for i, c := range columns {
		quoted[i] = quoteIdent(c)
		values[i] = "quote(" + quoteIdent(c) + ")"
	}
	prefix := "INSERT INTO " + quoteIdent(table) + " (" + strings.Join(quoted, ", ") + ") VALUES ("
	var rows []string
	if err := db.Raw(`SELECT ? || `+strings.Join(values, ` || ', ' || `)+` || ')' FROM `+quoteIdent(table)+` ORDER BY rowid`, prefix).
		Scan(&rows).Error; err != nil {
		return nil, fmt.Errorf("read rows of %s: %w", table, err)
	}
	return rows, nil
}

func quoteIdent(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}

func databaseIsEmpty(db *gorm.DB) (bool, error) {
	var n int64
	if err := db.Raw(`SELECT count(*) FROM sqlite_master WHERE name NOT LIKE 'sqlite_%'`).Scan(&n).Error; err != nil {
		return false, fmt.Errorf("inspect database: %w", err)
	}
	return n == 0, nil
}
