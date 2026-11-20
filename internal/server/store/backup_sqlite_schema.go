package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"os"
	"path/filepath"
)

// ErrBackupSQLiteCorrupt identifies a database that failed SQLite's physical
// integrity check. Unknown application migration markers use ErrSchemaIncompatible.
var ErrBackupSQLiteCorrupt = errors.New("backup SQLite database is corrupt")

// InspectBackupSQLiteSchema checks physical integrity and an exact known
// migration prefix in one read snapshot. Historical schemas are reported without
// migrating or seeding them. Committed WAL is included; the caller owns the
// offline scratch database and any sidecars, not a live serving Store.
func InspectBackupSQLiteSchema(ctx context.Context, path string) (int, error) {
	if ctx == nil {
		ctx = context.Background()
	}
	if err := ctx.Err(); err != nil {
		return 0, err
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return 0, err
	}
	info, err := os.Lstat(absolute)
	if err != nil {
		return 0, err
	}
	if !info.Mode().IsRegular() {
		return 0, errors.New("backup SQLite source must be a regular file")
	}
	db, err := openSQLiteImportReader(ctx, absolute, false)
	if err != nil {
		return 0, backupSQLiteIntegrityError(ctx, err)
	}
	defer db.Close()
	connection, err := db.Conn(ctx)
	if err != nil {
		return 0, backupSQLiteIntegrityError(ctx, err)
	}
	defer connection.Close()
	if _, err := connection.ExecContext(ctx, "PRAGMA query_only=ON"); err != nil {
		return 0, backupSQLiteIntegrityError(ctx, err)
	}
	tx, err := connection.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		return 0, backupSQLiteIntegrityError(ctx, err)
	}
	defer func() { _ = tx.Rollback() }()
	if err := checkBackupSQLiteIntegrity(ctx, tx); err != nil {
		return 0, err
	}
	version, err := readBackupSQLiteMigrationPrefix(ctx, tx)
	if err != nil {
		return version, err
	}
	if err := tx.Commit(); err != nil {
		return version, backupSQLiteIntegrityError(ctx, err)
	}
	return version, nil
}

func backupSQLiteIntegrityError(ctx context.Context, err error) error {
	if cause := ctx.Err(); cause != nil {
		return errors.Join(cause, err)
	}
	return fmt.Errorf("%w: %w", ErrBackupSQLiteCorrupt, err)
}

func checkBackupSQLiteIntegrity(ctx context.Context, tx *sql.Tx) error {
	rows, err := tx.QueryContext(ctx, "PRAGMA integrity_check")
	if err != nil {
		return backupSQLiteIntegrityError(ctx, err)
	}
	defer rows.Close()
	count := 0
	for rows.Next() {
		var result string
		if err := rows.Scan(&result); err != nil {
			return backupSQLiteIntegrityError(ctx, err)
		}
		count++
		if result != "ok" {
			return fmt.Errorf("%w: SQLite integrity check: %s", ErrBackupSQLiteCorrupt, result)
		}
	}
	if err := rows.Err(); err != nil {
		return backupSQLiteIntegrityError(ctx, err)
	}
	if count != 1 {
		return fmt.Errorf("%w: incomplete integrity result", ErrBackupSQLiteCorrupt)
	}
	return nil
}

func readBackupSQLiteMigrationPrefix(ctx context.Context, tx *sql.Tx) (int, error) {
	var exists int
	if err := tx.QueryRowContext(ctx, "SELECT count(*) FROM sqlite_schema WHERE type='table' AND name='schema_migrations'").Scan(&exists); err != nil {
		return 0, backupSQLiteIntegrityError(ctx, err)
	}
	if exists != 1 {
		return 0, fmt.Errorf("%w: backup has no migration marker table", ErrSchemaIncompatible)
	}
	rows, err := tx.QueryContext(ctx, "SELECT version, name FROM schema_migrations ORDER BY version LIMIT ?", len(migrations)+1)
	if err != nil {
		if ctx.Err() != nil {
			return 0, errors.Join(ctx.Err(), err)
		}
		return 0, fmt.Errorf("%w: read backup migration markers: %w", ErrSchemaIncompatible, err)
	}
	defer rows.Close()
	observed, count := 0, 0
	mismatch := false
	for rows.Next() {
		var version int
		var name string
		if err := rows.Scan(&version, &name); err != nil {
			return observed, fmt.Errorf("%w: invalid backup migration marker: %w", ErrSchemaIncompatible, err)
		}
		if version > observed {
			observed = version
		}
		if count >= len(migrations) || version != migrations[count].Version || name != migrations[count].Name {
			mismatch = true
		}
		count++
	}
	if err := rows.Err(); err != nil {
		if ctx.Err() != nil {
			return observed, errors.Join(ctx.Err(), err)
		}
		return observed, fmt.Errorf("%w: read backup migration markers: %w", ErrSchemaIncompatible, err)
	}
	if count == 0 || mismatch {
		return observed, fmt.Errorf("%w: backup migration markers are not a complete known prefix", ErrSchemaIncompatible)
	}
	return observed, nil
}
