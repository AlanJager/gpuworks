package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"gorm.io/gorm"
)

// ImportSQLite stages a current-version SQLite database in an empty store.
// The caller must authorize maintenance, keep the target out of service, retain
// the original encryption materials, and call InitializeSchema before serving.
// This copies database state only; it neither creates keys nor starts recovery.
// A verified backup is retained even if the target transaction fails. A commit
// error may have an unknown outcome and must not be retried without inspection.
func (s *Store) ImportSQLite(ctx context.Context, sourcePath, backupPath string) error {
	if s == nil || s.db == nil {
		return errors.New("SQLite import requires an open target store")
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	sourcePath, err := filepath.Abs(sourcePath)
	if err != nil {
		return fmt.Errorf("SQLite import source path: %w", err)
	}
	backupPath, err = filepath.Abs(backupPath)
	if err != nil {
		return fmt.Errorf("SQLite import backup path: %w", err)
	}
	info, err := os.Stat(sourcePath)
	if err != nil || !info.Mode().IsRegular() {
		if err == nil {
			err = errors.New("source must be a regular database file")
		}
		return fmt.Errorf("SQLite import source: %w", err)
	}
	source, err := openSQLiteImportReader(ctx, sourcePath, false)
	if err != nil {
		return err
	}
	defer source.Close()
	if _, err := inspectSQLiteImport(ctx, source); err != nil {
		return fmt.Errorf("SQLite import source unsupported: %w", err)
	}
	if err := createSQLiteImportBackup(ctx, source, backupPath); err != nil {
		return fmt.Errorf("SQLite import backup failed; target untouched: %w", err)
	}
	backup, err := openSQLiteImportReader(ctx, backupPath, true)
	if err != nil {
		return fmt.Errorf("SQLite import backup retained at %s: %w", backupPath, err)
	}
	defer backup.Close()
	schema, err := inspectSQLiteImport(ctx, backup)
	if err != nil {
		return fmt.Errorf("SQLite import backup validation failed at %s: %w", backupPath, err)
	}
	target := s.db
	if s.schedulingDB != nil {
		target = s.schedulingDB
	}
	// As for a restored image: an empty target, out of service.
	if err := importSQLiteSnapshot(withoutWriterGuard(ctx), target, backup, schema); err != nil {
		return fmt.Errorf("SQLite import failed; backup retained at %s: %w", backupPath, err)
	}
	return nil
}

func openSQLiteImportReader(ctx context.Context, path string, immutable bool) (*sql.DB, error) {
	u := url.URL{Scheme: "file", Path: path}
	q := url.Values{"mode": {"ro"}, "_pragma": {"busy_timeout(5000)", "trusted_schema(0)"}}
	if immutable {
		q.Set("immutable", "1")
		q.Add("_pragma", "query_only(1)")
	}
	u.RawQuery = q.Encode()
	db, err := sql.Open("sqlite", u.String())
	if err != nil {
		return nil, fmt.Errorf("open SQLite import reader: %w", err)
	}
	db.SetMaxOpenConns(1)
	if err := db.PingContext(ctx); err != nil {
		db.Close()
		return nil, fmt.Errorf("read SQLite import database: %w", err)
	}
	return db, nil
}

func createSQLiteImportBackup(ctx context.Context, source *sql.DB, path string) error {
	file, err := os.OpenFile(path, os.O_CREATE|os.O_EXCL|os.O_RDWR, 0o600)
	if err != nil {
		return fmt.Errorf("create new backup %s: %w", path, err)
	}
	defer file.Close()
	// INTO writes only the separately created output, including committed WAL
	// records. Do not use immutable=1 on the live source or copy its main file.
	if _, err := source.ExecContext(ctx, "VACUUM main INTO ?", path); err != nil {
		return fmt.Errorf("incomplete backup retained at %s: %w", path, err)
	}
	if err := file.Sync(); err != nil {
		return fmt.Errorf("sync backup: %w", err)
	}
	dir, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("open backup directory for sync: %w", err)
	}
	defer dir.Close()
	if err := dir.Sync(); err != nil {
		return fmt.Errorf("sync backup directory: %w", err)
	}
	return nil
}

func importSQLiteSnapshot(ctx context.Context, target *gorm.DB, source *sql.DB, schema sqliteImportSchema) error {
	return target.WithContext(ctx).Transaction(func(tx *gorm.DB) error {
		var count int64
		if err := tx.Raw("SELECT count(*) FROM sqlite_schema").Scan(&count).Error; err != nil {
			return fmt.Errorf("inspect import target: %w", err)
		}
		if count != 0 {
			return errors.New("SQLite import target must be empty; existing objects are never replaced")
		}
		// Keep any enabled FK checks, but defer them until every table is loaded.
		if err := tx.Exec("PRAGMA defer_foreign_keys = ON").Error; err != nil {
			return err
		}
		for _, table := range schema.tables {
			if err := tx.Exec(table.ddl).Error; err != nil {
				return fmt.Errorf("create import table %q: %w", table.name, err)
			}
		}
		for _, table := range schema.tables {
			if err := copySQLiteImportTable(ctx, source, tx, table); err != nil {
				return err
			}
		}
		if schema.sequence {
			if err := tx.Exec("DELETE FROM sqlite_sequence").Error; err != nil {
				return fmt.Errorf("reset imported sequences: %w", err)
			}
			if err := copySQLiteImportTable(ctx, source, tx, sqliteImportTable{
				name: "sqlite_sequence", columns: []string{"name", "seq"},
			}); err != nil {
				return err
			}
		}
		for _, index := range schema.indexes {
			if err := tx.Exec(index.ddl).Error; err != nil {
				return fmt.Errorf("create import index %q: %w", index.name, err)
			}
		}
		for _, trigger := range schema.triggers {
			// Replay the executable's own trigger text. A same-named trigger with
			// any other body was already rejected, so this cannot run source SQL.
			sql, ok := schemaTrigger(trigger.name)
			if !ok {
				return fmt.Errorf("create import trigger %q: unknown", trigger.name)
			}
			if err := tx.Exec(sql).Error; err != nil {
				return fmt.Errorf("create import trigger %q: %w", trigger.name, err)
			}
		}
		for _, value := range []struct {
			name  string
			value int64
		}{{"user_version", schema.userVersion}, {"application_id", schema.applicationID}} {
			if err := tx.Exec("PRAGMA " + value.name + " = " + strconv.FormatInt(value.value, 10)).Error; err != nil {
				return fmt.Errorf("restore SQLite %s: %w", value.name, err)
			}
		}
		return ctx.Err()
	})
}

func copySQLiteImportTable(ctx context.Context, source *sql.DB, tx *gorm.DB, table sqliteImportTable) error {
	quoted := make([]string, len(table.columns))
	selects := make([]string, len(table.columns))
	for i, name := range table.columns {
		quoted[i] = quoteSQLiteImportName(name)
		// Expressions prevent the source driver from normalizing DATETIME text
		// into time.Time. Reading text as bytes also preserves embedded NULs.
		selects[i] = "typeof(" + quoted[i] + "), CASE WHEN typeof(" + quoted[i] + ") = 'text' THEN CAST(" + quoted[i] + " AS BLOB) ELSE " + quoted[i] + " END"
	}
	rows, err := source.QueryContext(ctx, "SELECT "+strings.Join(selects, ",")+" FROM "+quoteSQLiteImportName(table.name))
	if err != nil {
		return fmt.Errorf("read import table %q: %w", table.name, err)
	}
	defer rows.Close()
	for rows.Next() {
		values := make([]any, len(table.columns)*2)
		pointers := make([]any, len(values))
		for i := range pointers {
			pointers[i] = &values[i]
		}
		if err := rows.Scan(pointers...); err != nil {
			return fmt.Errorf("scan import table %q: %w", table.name, err)
		}
		args := make([]any, len(table.columns))
		placeholders := make([]string, len(table.columns))
		for i := range args {
			args[i] = values[2*i+1]
			placeholders[i] = "?"
			if values[2*i] == "text" || values[2*i] == "blob" {
				if bytes, ok := args[i].([]byte); ok && len(bytes) == 0 {
					// The source driver returns a nil slice for an empty blob;
					// binding that nil slice would turn empty text/blob into NULL.
					args[i] = []byte{}
				}
			}
			if values[2*i] == "text" {
				// dqlite string parameters are NUL-terminated. Bind a blob and
				// cast in SQLite so arbitrary text bytes survive the protocol.
				placeholders[i] = "CAST(? AS TEXT)"
			}
		}
		query := "INSERT INTO " + quoteSQLiteImportName(table.name) + " (" + strings.Join(quoted, ",") + ") VALUES (" + strings.Join(placeholders, ",") + ")"
		// Use the current transaction directly: GORM's raw-expression parser
		// treats ? inside quoted identifiers as placeholders and can expand
		// byte slices in CAST(? AS TEXT) into a list instead of a single blob.
		if _, err := tx.Statement.ConnPool.ExecContext(ctx, query, args...); err != nil {
			return fmt.Errorf("copy import table %q: %w", table.name, err)
		}
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("read import table %q: %w", table.name, err)
	}
	return nil
}

func quoteSQLiteImportName(name string) string {
	return `"` + strings.ReplaceAll(name, `"`, `""`) + `"`
}
