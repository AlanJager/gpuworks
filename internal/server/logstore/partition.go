package logstore

import (
	"database/sql"
	"fmt"
	"strings"
	"time"
)

// A partition is one UTC day of logs: a plain table plus an external-content
// FTS5 index over the line text, kept in sync by triggers. Partition names are
// their UTC day (logs_20260919), so name order is day order.

const partitionPrefix = "logs_"

// partitionDay is the UTC day a timestamp belongs to.
func partitionDay(tsMS int64) string {
	return time.UnixMilli(tsMS).UTC().Format("20060102")
}

// partitionName is the meta table of the partition for a timestamp.
func partitionName(tsMS int64) string {
	return partitionPrefix + partitionDay(tsMS)
}

// partitionStartMS is the first millisecond of a partition's UTC day.
func partitionStartMS(day string) (int64, error) {
	t, err := time.ParseInLocation("20060102", day, time.UTC)
	if err != nil {
		return 0, fmt.Errorf("parse partition day %q: %w", day, err)
	}
	return t.UnixMilli(), nil
}

// txExec is the subset of database/sql handles DDL and DML run against —
// either the writer pool or a transaction.
type txExec interface {
	Exec(query string, args ...any) (sql.Result, error)
}

// ensurePartition creates the partition for a timestamp if absent. Safe to
// call concurrently (IF NOT EXISTS everywhere) and inside the migration
// transaction. The s-partition form short-circuits on the ready memo; the
// free function stays memo-less for callers (the migration) that must be
// certain the tables exist inside their own transaction.
func (s *sqliteStore) ensurePartition(e txExec, tsMS int64) error {
	day := partitionDay(tsMS)
	if _, ok := s.partitionsReady.Load(day); ok {
		return nil
	}
	if err := ensurePartition(e, tsMS); err != nil {
		return err
	}
	s.partitionsReady.Store(day, struct{}{})
	return nil
}

func ensurePartition(e txExec, tsMS int64) error {
	name := partitionName(tsMS)
	if _, err := e.Exec(fmt.Sprintf(`
		CREATE TABLE IF NOT EXISTS %[1]s (
			id INTEGER PRIMARY KEY,
			ts INTEGER NOT NULL,
			instance TEXT NOT NULL DEFAULT '',
			node TEXT NOT NULL DEFAULT '',
			level TEXT NOT NULL DEFAULT '',
			source TEXT NOT NULL DEFAULT 'instance',
			line TEXT NOT NULL
		);`, name)); err != nil {
		return err
	}
	for _, idx := range []string{
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %[1]s_ts ON %[1]s(ts, id);`, name),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %[1]s_inst ON %[1]s(instance, ts, id);`, name),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %[1]s_node ON %[1]s(node, ts, id);`, name),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %[1]s_level ON %[1]s(level, ts, id);`, name),
		fmt.Sprintf(`CREATE INDEX IF NOT EXISTS %[1]s_src ON %[1]s(source, ts, id);`, name),
	} {
		if _, err := e.Exec(idx); err != nil {
			return err
		}
	}
	if _, err := e.Exec(fmt.Sprintf(
		`CREATE VIRTUAL TABLE IF NOT EXISTS %[1]s_fts USING fts5(line, content='%[1]s', content_rowid='id');`, name)); err != nil {
		return err
	}
	// The external-content FTS5 table has no storage of its own; these triggers
	// mirror the line column on every meta-table write.
	triggers := []string{
		fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS %[1]s_fts_ins AFTER INSERT ON %[1]s BEGIN
			INSERT INTO %[1]s_fts(rowid, line) VALUES (new.id, new.line);
		END;`, name),
		fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS %[1]s_fts_del AFTER DELETE ON %[1]s BEGIN
			INSERT INTO %[1]s_fts(%[1]s_fts, rowid, line) VALUES ('delete', old.id, old.line);
		END;`, name),
		fmt.Sprintf(`CREATE TRIGGER IF NOT EXISTS %[1]s_fts_upd AFTER UPDATE ON %[1]s BEGIN
			INSERT INTO %[1]s_fts(%[1]s_fts, rowid, line) VALUES ('delete', old.id, old.line);
			INSERT INTO %[1]s_fts(rowid, line) VALUES (new.id, new.line);
		END;`, name),
	}
	for _, trg := range triggers {
		if _, err := e.Exec(trg); err != nil {
			return err
		}
	}
	return nil
}

// listPartitions returns partition day names, newest first.
func listPartitions(db *sql.DB) ([]string, error) {
	rows, err := db.Query(fmt.Sprintf(
		`SELECT name FROM sqlite_master WHERE type='table' AND name GLOB '%s[0-9][0-9][0-9][0-9][0-9][0-9][0-9][0-9]' ORDER BY name DESC`,
		partitionPrefix))
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
		out = append(out, strings.TrimPrefix(name, partitionPrefix))
	}
	return out, rows.Err()
}

// restrictPartitions narrows the newest-first partition list to the days the
// filter (time range, cursor) can touch.
func restrictPartitions(days []string, f Filter) []string {
	fromDay, toDay, cursorDay := "", "", ""
	if f.FromMS > 0 {
		fromDay = partitionDay(f.FromMS)
	}
	if f.ToMS > 0 {
		toDay = partitionDay(f.ToMS)
	}
	if f.CursorTS > 0 {
		cursorDay = partitionDay(f.CursorTS)
	}
	out := make([]string, 0, len(days))
	for _, day := range days {
		if toDay != "" && day > toDay {
			continue
		}
		if fromDay != "" && day < fromDay {
			continue
		}
		// Rows older than the cursor cannot live in a day newer than the
		// cursor's own day.
		if cursorDay != "" && day > cursorDay {
			continue
		}
		out = append(out, day)
	}
	return out
}
