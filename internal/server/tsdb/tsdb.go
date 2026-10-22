// Package tsdb is a lightweight embedded time-series store for GPU, host, and
// vLLM metrics. It persists to its own SQLite file (separate from the main DB)
// via the pure-Go modernc driver and database/sql.
//
// The Store interface isolates the storage backend so a Prometheus or
// VictoriaMetrics implementation can replace the SQLite one later without
// touching callers.
package tsdb

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"sort"
	"strings"
	"time"

	_ "github.com/glebarez/go-sqlite" // registers the "sqlite" driver (shared with the main store; avoids double registration)
)

// Sample is a single metric observation to be written.
type Sample struct {
	Metric string            // e.g. "gpuworks_gpu_utilization"
	Labels map[string]string // e.g. {"node":"uuid","gpu":"0"}
	Value  float64
	TimeMS int64 // unix ms
}

// Point is one (time, value) pair in a Series.
type Point struct {
	TimeMS int64
	Value  float64
}

// Series is the points for one distinct label-set.
type Series struct {
	Labels map[string]string
	Points []Point
}

// LatestPoint is the newest sample for one distinct label set.
type LatestPoint struct {
	Labels map[string]string
	Value  float64
	TimeMS int64
}

// Store is the storage-backend-agnostic time-series interface.
type Store interface {
	WriteBatch(samples []Sample) error
	// QueryRange returns one Series per distinct label-set matching `metric`
	// and all `matchers` (exact label equality), with points bucketed to
	// stepMS (last value per bucket) over [startMS, endMS].
	QueryRange(metric string, matchers map[string]string, startMS, endMS, stepMS int64) ([]Series, error)
	// QueryLatest returns one newest sample per matching label set, at or after
	// sinceMS and, when supplied, at or before the single optional asOfMS.
	QueryLatest(metric string, matchers map[string]string, sinceMS int64, asOfMS ...int64) ([]LatestPoint, error)
	// Retain deletes samples older than the cutoff (unix ms), and also drops
	// any series left with no samples remaining. It honours ctx between
	// batches so a caller shutting down is not held up by a large backlog.
	Retain(ctx context.Context, olderThanMS int64) error
	Close() error
}

// sqliteStore is the SQLite-backed Store implementation.
type sqliteStore struct {
	db *sql.DB
	// retainBatchHook, when set, is called with the rows-affected count after
	// each Retain delete pass. Test-only observability (same-package tests set
	// it directly); nil in production.
	retainBatchHook func(deleted int64)
}

// OpenSQLite opens (creating if needed) a metrics database at path and returns
// a ready Store. WAL and a busy timeout are enabled; the connection pool is
// capped at one since SQLite has a single writer.
func OpenSQLite(path string) (Store, error) {
	dsn := path + "?_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)&_pragma=synchronous(NORMAL)"
	db, err := sql.Open("sqlite", dsn)
	if err != nil {
		return nil, fmt.Errorf("open sqlite: %w", err)
	}
	db.SetMaxOpenConns(1)
	s := &sqliteStore{db: db}
	if err := s.migrate(); err != nil {
		_ = db.Close()
		return nil, err
	}
	return s, nil
}

// migrate brings the on-disk schema up to date. See schema.go for the
// versioned migration list; this is a thin wrapper so tests can drive
// migrateSchema against a partial list without going through OpenSQLite.
func (s *sqliteStore) migrate() error {
	return migrateSchema(s.db, schemaMigrations)
}

// labelsKey builds a stable identity for a series: metric plus its labels
// rendered as sorted "k=v" pairs. Two samples with the same metric and labels
// (in any map order) always produce the same key.
func labelsKey(metric string, labels map[string]string) string {
	keys := make([]string, 0, len(labels))
	for k := range labels {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	var b strings.Builder
	b.WriteString(metric)
	for _, k := range keys {
		b.WriteByte(0x1f) // unit separator, avoids collisions with label text
		b.WriteString(k)
		b.WriteByte('=')
		b.WriteString(labels[k])
	}
	return b.String()
}

// nullableLabel returns labels[key] as a bind value, or nil (SQL NULL) when
// the label is absent or empty. Most metrics (GPU/host samples) carry neither
// "instance" nor "deployment", and NULL is what lets an indexed lookup on
// that column correctly find nothing rather than matching on "".
func nullableLabel(labels map[string]string, key string) any {
	if v, ok := labels[key]; ok && v != "" {
		return v
	}
	return nil
}

// WriteBatch upserts each sample's series (get-or-create by labels_key) and
// inserts the samples in a single transaction. Duplicate (series, ts) rows are
// replaced (last write wins).
func (s *sqliteStore) WriteBatch(samples []Sample) error {
	if len(samples) == 0 {
		return nil
	}
	tx, err := s.db.Begin()
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()

	// ON CONFLICT self-heals a series row whose instance/deployment columns
	// were left NULL by an older binary (the rollback window right after an
	// upgrade, or any row written before migration 2 populated them): the
	// next write for that exact label set repairs the columns in place
	// instead of leaving them permanently invisible to the pushed-down
	// matcher in QueryRange/QueryLatest. The WHERE guard makes the UPDATE a
	// no-op (no write, no row version bump) once the columns already agree,
	// which is the common case on every write after the first repair.
	insSeries, err := tx.Prepare(`
		INSERT INTO series(metric, labels_key, labels_json, instance, deployment)
		VALUES(?, ?, ?, ?, ?)
		ON CONFLICT(labels_key) DO UPDATE SET
			instance = excluded.instance,
			deployment = excluded.deployment
		WHERE series.instance IS NOT excluded.instance OR series.deployment IS NOT excluded.deployment`)
	if err != nil {
		return err
	}
	defer insSeries.Close()
	selSeries, err := tx.Prepare(`SELECT id FROM series WHERE labels_key = ?`)
	if err != nil {
		return err
	}
	defer selSeries.Close()
	insSample, err := tx.Prepare(
		`INSERT OR REPLACE INTO samples(series_id, ts_ms, value) VALUES(?, ?, ?)`)
	if err != nil {
		return err
	}
	defer insSample.Close()

	// Cache labels_key -> series id within this batch to cut round-trips.
	ids := make(map[string]int64)
	for _, sm := range samples {
		key := labelsKey(sm.Metric, sm.Labels)
		id, ok := ids[key]
		if !ok {
			lj, err := json.Marshal(sm.Labels)
			if err != nil {
				return err
			}
			instance := nullableLabel(sm.Labels, "instance")
			deployment := nullableLabel(sm.Labels, "deployment")
			if _, err := insSeries.Exec(sm.Metric, key, string(lj), instance, deployment); err != nil {
				return err
			}
			if err := selSeries.QueryRow(key).Scan(&id); err != nil {
				return err
			}
			ids[key] = id
		}
		if _, err := insSample.Exec(id, sm.TimeMS, sm.Value); err != nil {
			return err
		}
	}
	return tx.Commit()
}

// queryRangeJoinSQL is QueryRange's single statement: one LEFT JOIN pass over
// every candidate series and its samples in [startMS, endMS], streamed in
// series_id, ts_ms order so Go can bucket each series as its rows arrive
// instead of issuing one SELECT per series (the N+1 this replaces —
// TestQueryRangeSingleStatementPlanSeeksSamplesBySeriesID pins the plan). The
// LEFT JOIN (not an INNER JOIN) is deliberate: a candidate series with zero
// samples in the window must still produce one row (with NULL sm columns) so
// it is returned with empty Points, matching the pre-single-statement
// behavior that queried candidates independently of whether they had any
// samples in range.
const queryRangeJoinSQL = `
		SELECT sr.id, sr.labels_json, sm.ts_ms, sm.value
		FROM series sr
		LEFT JOIN samples sm ON sm.series_id = sr.id AND sm.ts_ms >= ? AND sm.ts_ms <= ?
		WHERE sr.metric = ?%s
		ORDER BY sr.id, sm.ts_ms`

// QueryRange finds every series whose metric equals `metric` and whose labels
// are a superset of `matchers`, then returns bucketed points for each. An
// "instance" and/or "deployment" matcher is pushed into the SQL WHERE against
// the series table's indexed columns (see schema.go migration 2); every other
// matcher is still checked exactly in Go against the decoded labels_json,
// same as before that index existed.
func (s *sqliteStore) QueryRange(metric string, matchers map[string]string, startMS, endMS, stepMS int64) ([]Series, error) {
	if stepMS <= 0 {
		return nil, fmt.Errorf("stepMS must be positive, got %d", stepMS)
	}
	clause, extraArgs, rest := indexedMatcherClause("sr.", matchers)
	query := fmt.Sprintf(queryRangeJoinSQL, clause)
	args := append([]any{startMS, endMS, metric}, extraArgs...)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()

	// Both slices are pre-allocated non-nil (rather than left as a zero-value
	// `var`) so an empty result - no matching series, or a matching series
	// with no samples in the window - marshals to JSON as `[]`, not `null`:
	// this is a wire contract callers (the monitor/monitorapi dashboards)
	// round-trip through encoding/json, and `null` there reads as "missing"
	// rather than "empty".
	out := make([]Series, 0, 4)
	var curID int64 = -1
	var curLabels map[string]string
	var curMatches bool
	var curPoints []Point
	haveBucket := false
	var curBucket int64
	flush := func() {
		if curID != -1 && curMatches {
			out = append(out, Series{Labels: curLabels, Points: curPoints})
		}
	}
	for rows.Next() {
		var id int64
		var lj string
		var ts sql.NullInt64
		var val sql.NullFloat64
		if err := rows.Scan(&id, &lj, &ts, &val); err != nil {
			return nil, err
		}
		if id != curID {
			flush()
			curID = id
			labels := map[string]string{}
			if err := json.Unmarshal([]byte(lj), &labels); err != nil {
				return nil, err
			}
			curLabels = labels
			curMatches = matchesAll(labels, rest)
			curPoints = make([]Point, 0, 4)
			haveBucket = false
		}
		if !curMatches || !ts.Valid {
			// !ts.Valid is the LEFT JOIN's "no sample in window" row for this
			// series: it carries no point, but the series (and its empty
			// Points) still gets flushed above when the next id starts.
			continue
		}
		bucket := (ts.Int64 / stepMS) * stepMS
		if haveBucket && bucket == curBucket {
			// Same bucket: rows are time-ordered, so overwrite with later value.
			curPoints[len(curPoints)-1].Value = val.Float64
			continue
		}
		curPoints = append(curPoints, Point{TimeMS: bucket, Value: val.Float64})
		curBucket = bucket
		haveBucket = true
	}
	flush()
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// queryLatestBaseSQL and queryLatestOrderSQL are split so QueryLatest can
// splice indexed "instance"/"deployment" conditions in between them; tests
// exercise both the unfiltered and filtered forms built from these two
// (TestQueryLatestPlanSeeksPerSeriesTimeIndex).
const queryLatestBaseSQL = `
		SELECT sr.labels_json, sm.value, sm.ts_ms
		FROM series sr
		JOIN samples sm ON sm.series_id = sr.id AND sm.ts_ms = (
			SELECT MAX(recent.ts_ms)
			FROM samples recent
			WHERE recent.series_id = sr.id AND recent.ts_ms >= ? AND recent.ts_ms <= ?
		)
		WHERE sr.metric = ?`
const queryLatestOrderSQL = ` ORDER BY sr.id`

// QueryLatest returns the newest sample at or after sinceMS for each matching
// series. An optional asOfMS bounds the samples inclusively before selecting
// the latest; without it, the upper bound remains unrestricted. Like
// QueryRange, an "instance"/"deployment" matcher is pushed into the SQL WHERE
// against the series table's indexed columns; every other matcher is still
// checked exactly against the decoded labels_json.
func (s *sqliteStore) QueryLatest(metric string, matchers map[string]string, sinceMS int64, asOfMS ...int64) ([]LatestPoint, error) {
	if len(asOfMS) > 1 {
		return nil, fmt.Errorf("QueryLatest accepts at most one as-of timestamp")
	}
	endMS := int64(1<<63 - 1)
	if len(asOfMS) == 1 {
		endMS = asOfMS[0]
	}
	clause, extraArgs, rest := indexedMatcherClause("sr.", matchers)
	query := queryLatestBaseSQL + clause + queryLatestOrderSQL
	args := append([]any{sinceMS, endMS, metric}, extraArgs...)
	rows, err := s.db.Query(query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []LatestPoint
	for rows.Next() {
		var labelsJSON string
		var point LatestPoint
		if err := rows.Scan(&labelsJSON, &point.Value, &point.TimeMS); err != nil {
			return nil, err
		}
		point.Labels = map[string]string{}
		if err := json.Unmarshal([]byte(labelsJSON), &point.Labels); err != nil {
			return nil, err
		}
		if matchesAll(point.Labels, rest) {
			out = append(out, point)
		}
	}
	return out, rows.Err()
}

// matchesAll reports whether labels contains every key/value pair in matchers.
func matchesAll(labels, matchers map[string]string) bool {
	for k, v := range matchers {
		if labels[k] != v {
			return false
		}
	}
	return true
}

// indexedMatcherClause pulls the "instance" and "deployment" matchers - the
// two labels the series table carries as dedicated indexed columns (schema.go
// migration 2) - out of matchers and turns them into SQL conditions. prefix is
// the series table's alias in the calling query ("" for a bare "series",
// "sr." when it is joined and aliased). The remaining matchers are returned
// unchanged for the caller's Go-side matchesAll check.
func indexedMatcherClause(prefix string, matchers map[string]string) (clause string, args []any, rest map[string]string) {
	if len(matchers) == 0 {
		return "", nil, nil
	}
	rest = make(map[string]string, len(matchers))
	var b strings.Builder
	for _, key := range [...]string{"instance", "deployment"} {
		v, ok := matchers[key]
		if !ok {
			continue
		}
		if v == "" {
			// matchesAll treats an absent label as "" against an empty
			// matcher value, so {"instance": ""} matches a series with no
			// instance label at all. SQL cannot express that the same way:
			// "instance = ''" is never true for a NULL column. Leave an
			// empty-valued matcher for the Go-side exact check instead of
			// pushing it down.
			rest[key] = v
			continue
		}
		b.WriteString(" AND ")
		b.WriteString(prefix)
		b.WriteString(key)
		b.WriteString(" = ?")
		args = append(args, v)
	}
	for k, v := range matchers {
		if k == "instance" || k == "deployment" {
			continue
		}
		rest[k] = v
	}
	return b.String(), args, rest
}

// retainDropOrphanSeriesSQL deletes every series row with no samples left. A
// correlated NOT EXISTS lets the planner seek samples(series_id) once per
// series row via its (series_id, ts_ms) primary-key index; the equivalent
// "id NOT IN (SELECT DISTINCT series_id FROM samples)" instead forces a full
// scan/materialization of that subquery (measured ~10x slower, 691ms as a
// no-op at 160k samples). Kept as its own constant so its query plan is
// asserted on directly (TestRetainOrphanSweepPlanSeeksSamplesBySeriesID).
const retainDropOrphanSeriesSQL = `DELETE FROM series WHERE NOT EXISTS (SELECT 1 FROM samples WHERE samples.series_id = series.id)`

// retainBatchSize bounds each Retain delete pass so a large prune never holds
// the pool's single connection long enough to starve a concurrent writer or
// reader; retainBatchYield is the pause between passes that gives one a fair
// chance at the connection before the next batch starts. retainMaxBatches
// bounds the whole call: a truly pathological backlog (a server down for a
// long stretch, or the first prune after lowering the configured retention -
// see docs/reference/configuration.md's upgrade note) could otherwise turn
// one Retain call into an unbounded loop; hitting it is logged (not an
// error) and simply leaves the rest for the next scheduled call
// (observability.retainLoop runs this every 6 hours). All three are vars
// (not consts) so tests can shrink them to exercise multi-batch and
// hit-the-ceiling behavior without a huge table.
var (
	retainBatchSize  int64 = 50_000
	retainBatchYield       = 5 * time.Millisecond
	retainMaxBatches       = 10_000
)

// Retain deletes samples older than the cutoff (unix ms) in bounded batches,
// then drops any series left with no samples at all. Each batch is its own
// implicit transaction (there is no explicit Begin/Commit spanning batches),
// so between passes the pool's one connection is free for a queued WriteBatch
// or query — the previous single-transaction delete held that connection for
// the whole prune, which on a large table starved every writer and reader
// behind it until busy_timeout gave up. The orphan-series sweep runs once,
// after every sample batch is gone (or the batch ceiling is hit — a
// still-referenced series is harmless, it just isn't swept until a later
// call finishes the job): a series row is identity plus labels, and one
// with zero remaining samples is pure bookkeeping cruft that would
// otherwise accumulate forever and show up as an empty result in
// QueryRange/QueryLatest.
func (s *sqliteStore) Retain(ctx context.Context, olderThanMS int64) error {
	for batches := 0; ; batches++ {
		if ctx.Err() != nil {
			// A caller cancelling shutdown should not be held up by a large
			// backlog; the next scheduled run picks up where this left off,
			// the same as hitting retainMaxBatches below.
			return ctx.Err()
		}
		if batches >= retainMaxBatches {
			slog.Warn("tsdb retain hit its per-call batch ceiling; resuming on the next scheduled run",
				"batches", batches, "batch_size", retainBatchSize)
			break
		}
		res, err := s.db.Exec(
			`DELETE FROM samples WHERE rowid IN (SELECT rowid FROM samples WHERE ts_ms < ? LIMIT ?)`,
			olderThanMS, retainBatchSize)
		if err != nil {
			return err
		}
		n, err := res.RowsAffected()
		if err != nil {
			return err
		}
		if s.retainBatchHook != nil {
			s.retainBatchHook(n)
		}
		if n < retainBatchSize {
			break
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(retainBatchYield):
		}
	}
	_, err := s.db.Exec(retainDropOrphanSeriesSQL)
	return err
}

// Close releases the database handle.
func (s *sqliteStore) Close() error { return s.db.Close() }
