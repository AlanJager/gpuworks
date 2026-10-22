package uxstore

import (
	"database/sql"
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/uxrecord"
)

// Schema migrations. Each step rewrites the day tables an older file holds;
// tables created later come from partitionDDL in the current shape.
//
// Version 2 told the gateway's own share (receipt to dispatch) apart from the
// engine's preprocessing (uxrecord.SegmentPreprocess). Before it, the
// latency sums' gateway_ms and histogram row 0 held the two together; the
// gateway's part, a few milliseconds a request, cannot be recovered, so the
// whole of it becomes preprocessing: gateway_ms is renamed preprocess_ms and a
// gateway_ms of 0 is added, and row 0's cells move to the preprocessing row.
// A release from before version 2 refuses the migrated file.

// migrate brings a file at version from to schemaVersion in one transaction.
func migrate(db *sql.DB, from int) error {
	tx, err := db.Begin()
	if err != nil {
		return fmt.Errorf("migrate ux store: %w", err)
	}
	defer func() { _ = tx.Rollback() }()
	if from < 2 {
		if err := migrateGatewaySplit(tx); err != nil {
			return fmt.Errorf("migrate ux store to version 2: %w", err)
		}
	}
	if _, err := tx.Exec(fmt.Sprintf(`PRAGMA user_version = %d`, schemaVersion)); err != nil {
		return fmt.Errorf("write schema version: %w", err)
	}
	return tx.Commit()
}

func migrateGatewaySplit(tx *sql.Tx) error {
	lats, err := tablesLike(tx, latPrefix)
	if err != nil {
		return err
	}
	for _, table := range lats {
		for _, stmt := range []string{
			`ALTER TABLE ` + table + ` RENAME COLUMN gateway_ms TO preprocess_ms`,
			`ALTER TABLE ` + table + ` ADD COLUMN gateway_ms REAL NOT NULL DEFAULT 0`,
		} {
			if _, err := tx.Exec(stmt); err != nil {
				return fmt.Errorf("%s: %w", table, err)
			}
		}
	}
	grids, err := tablesLike(tx, gridPrefix)
	if err != nil {
		return err
	}
	gateway := uxrecord.LatencyRow(string(uxrecord.SegmentGateway))
	preprocess := uxrecord.LatencyRow(string(uxrecord.SegmentPreprocess))
	for _, table := range grids {
		if _, err := tx.Exec(`UPDATE `+table+` SET x = ? WHERE kind = ? AND x = ?`, preprocess, uxrecord.GridSegments, gateway); err != nil {
			return fmt.Errorf("%s: %w", table, err)
		}
	}
	return nil
}

// tablesLike lists the day tables with a prefix, oldest first.
func tablesLike(tx *sql.Tx, prefix string) ([]string, error) {
	rows, err := tx.Query(`SELECT name FROM sqlite_master WHERE type='table' AND name GLOB ? ORDER BY name`,
		prefix+`[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]`)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []string
	for rows.Next() {
		var name string
		if err := rows.Scan(&name); err != nil {
			return nil, err
		}
		out = append(out, name)
	}
	return out, rows.Err()
}
