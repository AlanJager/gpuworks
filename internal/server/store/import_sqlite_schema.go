package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"strings"
)

type sqliteImportTable struct {
	name    string
	ddl     string
	columns []string
}

type sqliteImportIndex struct {
	name string
	ddl  string
}

type sqliteImportSchema struct {
	tables        []sqliteImportTable
	indexes       []sqliteImportIndex
	triggers      []sqliteImportIndex
	sequence      bool
	userVersion   int64
	applicationID int64
}

func inspectSQLiteImport(ctx context.Context, db *sql.DB) (sqliteImportSchema, error) {
	var schema sqliteImportSchema
	checks, err := db.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return schema, fmt.Errorf("SQLite integrity check: %w", err)
	}
	var results int
	for checks.Next() {
		var result string
		if err := checks.Scan(&result); err != nil {
			checks.Close()
			return schema, err
		}
		results++
		if result != "ok" {
			checks.Close()
			return schema, fmt.Errorf("SQLite integrity check failed: %s", result)
		}
	}
	err = checks.Err()
	checks.Close()
	if err != nil {
		return schema, fmt.Errorf("SQLite integrity check: %w", err)
	}
	if results != 1 {
		return schema, fmt.Errorf("incomplete SQLite integrity check: results=%d", results)
	}
	if err := validateSQLiteImportMigrations(ctx, db); err != nil {
		return schema, err
	}
	rows, err := db.QueryContext(ctx, "SELECT type, name, sql FROM sqlite_schema ORDER BY name")
	if err != nil {
		return schema, err
	}
	for rows.Next() {
		var kind, name string
		var ddl sql.NullString
		if err := rows.Scan(&kind, &name, &ddl); err != nil {
			rows.Close()
			return schema, err
		}
		if name == "sqlite_sequence" && kind == "table" {
			schema.sequence = true
			continue
		}
		if kind == "index" && strings.HasPrefix(name, "sqlite_autoindex_") && !ddl.Valid {
			continue // Recreated by the original PRIMARY KEY / UNIQUE constraint.
		}
		if kind == "trigger" && ddl.Valid && maintenanceTriggerImportable(name, ddl.String) {
			schema.triggers = append(schema.triggers, sqliteImportIndex{name: name, ddl: ddl.String})
			continue
		}
		if strings.HasPrefix(strings.ToLower(name), "sqlite_") || !ddl.Valid || (kind != "table" && kind != "index") {
			rows.Close()
			return schema, fmt.Errorf("unsupported SQLite object %q (%s); no objects are omitted", name, kind)
		}
		if err := validateSQLiteImportDDL(kind, ddl.String); err != nil {
			rows.Close()
			return schema, fmt.Errorf("unsupported SQLite object %q: %w", name, err)
		}
		if kind == "table" {
			schema.tables = append(schema.tables, sqliteImportTable{name: name, ddl: ddl.String})
		} else {
			schema.indexes = append(schema.indexes, sqliteImportIndex{name: name, ddl: ddl.String})
		}
	}
	err = rows.Err()
	rows.Close()
	if err != nil {
		return schema, err
	}
	for i := range schema.tables {
		columns, err := sqliteImportColumns(ctx, db, schema.tables[i].name)
		if err != nil {
			return schema, err
		}
		schema.tables[i].columns = columns
	}
	if err := db.QueryRowContext(ctx, "PRAGMA user_version").Scan(&schema.userVersion); err != nil {
		return schema, err
	}
	if err := db.QueryRowContext(ctx, "PRAGMA application_id").Scan(&schema.applicationID); err != nil {
		return schema, err
	}
	return schema, nil
}

func validateSQLiteImportMigrations(ctx context.Context, db *sql.DB) error {
	rows, err := db.QueryContext(ctx, "SELECT version FROM schema_migrations")
	if err != nil {
		return fmt.Errorf("SQLite import requires current schema_migrations: %w", err)
	}
	defer rows.Close()
	known := make(map[int]bool, len(migrations))
	var latest int
	for _, m := range migrations {
		known[m.Version] = false
		if m.Version > latest {
			latest = m.Version
		}
	}
	var sourceVersion int
	for rows.Next() {
		var version int
		if err := rows.Scan(&version); err != nil {
			return err
		}
		if version <= 0 || version > latest {
			return fmt.Errorf("unsupported SQLite schema version %d; current version is %d", version, latest)
		}
		if _, ok := known[version]; ok {
			known[version] = true
		}
		if version > sourceVersion {
			sourceVersion = version
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if sourceVersion != latest {
		return fmt.Errorf("SQLite schema version %d must be upgraded explicitly to %d before import", sourceVersion, latest)
	}
	for _, m := range migrations {
		if !known[m.Version] {
			return fmt.Errorf("SQLite import requires applied migration %d (%s)", m.Version, m.Name)
		}
	}
	return nil
}

func sqliteImportColumns(ctx context.Context, db *sql.DB, table string) ([]string, error) {
	rows, err := db.QueryContext(ctx, "SELECT name, hidden FROM pragma_table_xinfo(?) ORDER BY cid", table)
	if err != nil {
		return nil, fmt.Errorf("inspect import columns %q: %w", table, err)
	}
	defer rows.Close()
	var columns []string
	for rows.Next() {
		var name string
		var hidden int
		if err := rows.Scan(&name, &hidden); err != nil {
			return nil, err
		}
		if hidden != 0 {
			return nil, fmt.Errorf("unsupported generated or hidden column %q in %q", name, table)
		}
		columns = append(columns, name)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	if len(columns) == 0 {
		return nil, fmt.Errorf("SQLite import table %q has no supported columns", table)
	}
	return columns, nil
}

// sqlite_schema already contains parsed SQLite DDL. Still reject additional
// statements, including writable_schema tampering, before passing it to another
// driver. Quotes and comments can legitimately contain semicolons or keywords.
func maintenanceTriggerImportable(name, ddl string) bool {
	want, ok := maintenancePhaseTrigger(name)
	if !ok {
		return false
	}
	return strings.Join(strings.Fields(ddl), " ") == strings.Join(strings.Fields(want), " ")
}

func validateSQLiteImportDDL(kind, ddl string) error {
	if strings.IndexByte(ddl, 0) >= 0 {
		return errors.New("NUL in schema SQL is not supported")
	}
	var words []string
	ended := false
	for i := 0; i < len(ddl); {
		c := ddl[i]
		if strings.ContainsRune(" \t\r\n\f\v", rune(c)) {
			i++
			continue
		}
		if i+1 < len(ddl) && ddl[i:i+2] == "--" {
			i += 2
			for i < len(ddl) && ddl[i] != '\n' {
				i++
			}
			continue
		}
		if i+1 < len(ddl) && ddl[i:i+2] == "/*" {
			end := strings.Index(ddl[i+2:], "*/")
			if end < 0 {
				return errors.New("unterminated SQL comment")
			}
			i += end + 4
			continue
		}
		if ended {
			return errors.New("multiple SQL statements are not supported")
		}
		if c == ';' {
			ended = true
			i++
			continue
		}
		if c == '\'' || c == '"' || c == '`' || c == '[' {
			end := c
			if c == '[' {
				end = ']'
			}
			i++
			closed := false
			for i < len(ddl) {
				if ddl[i] == end {
					i++
					if end != ']' && i < len(ddl) && ddl[i] == end {
						i++
						continue
					}
					closed = true
					break
				}
				i++
			}
			if !closed {
				return errors.New("unterminated SQL quote")
			}
			continue
		}
		if (c >= 'a' && c <= 'z') || (c >= 'A' && c <= 'Z') || c == '_' {
			start := i
			for i < len(ddl) && ((ddl[i] >= 'a' && ddl[i] <= 'z') || (ddl[i] >= 'A' && ddl[i] <= 'Z') || ddl[i] == '_') {
				i++
			}
			if len(words) < 3 {
				words = append(words, strings.ToUpper(ddl[start:i]))
			}
			continue
		}
		i++
	}
	if len(words) < 2 || words[0] != "CREATE" {
		return errors.New("expected CREATE TABLE or CREATE INDEX")
	}
	if kind == "table" && words[1] == "TABLE" {
		return nil
	}
	if kind == "index" && (words[1] == "INDEX" || (len(words) >= 3 && words[1] == "UNIQUE" && words[2] == "INDEX")) {
		return nil
	}
	return errors.New("only ordinary CREATE TABLE and CREATE INDEX are supported")
}
