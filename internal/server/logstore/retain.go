package logstore

import (
	"context"
	"time"
)

// Checkpoint advances the WAL on the writer connection (PASSIVE: it never
// blocks behind readers and copies only what no live read snapshot pins).
// Without it, long reader transactions — a slow search holding a snapshot —
// let autocheckpoint stall and the WAL grow without bound, which makes every
// later commit and every later read walk more of it: the feedback loop the
// fleet run measured as histogram and keyword latency doubling between two
// rounds of the same load. Returns (busy, logPages, checkpointedPages).
func (s *sqliteStore) Checkpoint() (busy bool, logPages, checkpointed int64, err error) {
	var b, l, c int64
	err = s.writer.QueryRow(`PRAGMA wal_checkpoint(PASSIVE)`).Scan(&b, &l, &c)
	return b != 0, l, c, err
}

// Retain drops expired logs. Whole partitions strictly older than the cutoff
// are dropped (O(1) per partition); the boundary partition keeps at most one
// day of rows and is pruned in small batches so the writer never stalls long
// enough for ingest to back up. ctx is checked between partitions and between
// batches so a caller shutting down is not held up by a large backlog (a
// server down for a long stretch, or the first prune after lowering the
// configured retention); the rest resumes on the next scheduled call.
func (s *sqliteStore) Retain(ctx context.Context, olderThanMS int64) error {
	days, err := listPartitions(s.writer)
	if err != nil {
		return err
	}
	for _, day := range days {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		startMS, err := partitionStartMS(day)
		if err != nil {
			return err
		}
		endMS := startMS + (24 * time.Hour).Milliseconds()
		switch {
		case endMS <= olderThanMS:
			// Every row in the partition is expired: drop the pair. Dropping
			// the meta table takes its indexes and triggers with it.
			name := partitionPrefix + day
			if _, err := s.writer.Exec(`DROP TABLE IF EXISTS ` + name + `_fts`); err != nil {
				return err
			}
			if _, err := s.writer.Exec(`DROP TABLE IF EXISTS ` + name); err != nil {
				return err
			}
			s.partitionsReady.Delete(day)
		case startMS < olderThanMS:
			// Boundary partition: delete expired rows in batches. The DELETE
			// trigger keeps the FTS index in sync per row.
			const batch = 5000
			for {
				if ctx.Err() != nil {
					return ctx.Err()
				}
				res, err := s.writer.Exec(
					`DELETE FROM `+partitionPrefix+day+` WHERE id IN
					 (SELECT id FROM `+partitionPrefix+day+` WHERE ts < ? LIMIT ?)`,
					olderThanMS, batch)
				if err != nil {
					return err
				}
				n, _ := res.RowsAffected()
				if n < batch {
					break
				}
			}
		}
	}
	return nil
}
