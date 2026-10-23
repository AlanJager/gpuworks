package statsdb

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"gpuworks.ai/gpuworks/internal/server/rollup"
)

// AddQuality adds a delta to the hour rows. The histograms are merged in Go:
// each touched row is read and written back in one transaction, a few
// hundred primary-key reads for a whole fleet's five minutes.
func (s *Store) AddQuality(ctx context.Context, delta map[rollup.QualityKey]rollup.Quality) error {
	if len(delta) == 0 {
		return nil
	}
	return s.inTx(ctx, func(tx *sql.Tx) error { return addQualityTx(ctx, tx, delta) })
}

// AddQualityBackfill adds a delta filled in from the request log and records
// in the same transaction how far back the fill reaches.
func (s *Store) AddQualityBackfill(ctx context.Context, delta map[rollup.QualityKey]rollup.Quality, b Backfill) error {
	return s.inTx(ctx, func(tx *sql.Tx) error {
		if err := addQualityTx(ctx, tx, delta); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO backfill (kind, through_ms, done) VALUES (?, ?, ?)
			ON CONFLICT (kind) DO UPDATE SET through_ms = excluded.through_ms, done = excluded.done`, string(KindQuality), b.ThroughMS, b.Done)
		return err
	})
}

func addQualityTx(ctx context.Context, tx *sql.Tx, delta map[rollup.QualityKey]rollup.Quality) error {
	read, err := tx.PrepareContext(ctx, `SELECT requests, completed, length, client_closed, interrupted, error,
		ttft_hist, e2e_hist, output_tokens, decode_ms FROM quality_hourly WHERE hour_ms = ? AND deployment_id = ?`)
	if err != nil {
		return err
	}
	defer read.Close()
	write, err := tx.PrepareContext(ctx, `INSERT OR REPLACE INTO quality_hourly (hour_ms, deployment_id,
		requests, completed, length, client_closed, interrupted, error, ttft_hist, e2e_hist, output_tokens, decode_ms)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
	if err != nil {
		return err
	}
	defer write.Close()
	for k, d := range delta {
		cur, err := scanQuality(read.QueryRowContext(ctx, k.HourMS, k.DeploymentID))
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		cur.Add(d)
		if _, err := write.ExecContext(ctx, append([]any{k.HourMS, k.DeploymentID}, qualityArgs(cur)...)...); err != nil {
			return err
		}
	}
	return nil
}

type rowScanner interface{ Scan(dest ...any) error }

func scanQuality(row rowScanner) (rollup.Quality, error) {
	var q rollup.Quality
	var ttft, e2e []byte
	if err := row.Scan(&q.Requests, &q.Completed, &q.Length, &q.ClientClosed, &q.Interrupted, &q.Error,
		&ttft, &e2e, &q.OutputTokens, &q.DecodeMS); err != nil {
		return rollup.Quality{}, err
	}
	var err error
	if q.TTFT, err = rollup.DecodeHistogram(ttft); err != nil {
		return rollup.Quality{}, err
	}
	if q.EndToEnd, err = rollup.DecodeHistogram(e2e); err != nil {
		return rollup.Quality{}, err
	}
	return q, nil
}

func qualityArgs(q rollup.Quality) []any {
	return []any{q.Requests, q.Completed, q.Length, q.ClientClosed, q.Interrupted, q.Error,
		q.TTFT.Encode(), q.EndToEnd.Encode(), q.OutputTokens, q.DecodeMS}
}

// AddGPUs adds a delta to the hour rows in one transaction; models names
// each card's model (a card it does not name keeps the one it has).
func (s *Store) AddGPUs(ctx context.Context, delta map[rollup.GPUKey]rollup.GPU, models map[rollup.GPUKey]string) error {
	if len(delta) == 0 {
		return nil
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO gpu_hourly (hour_ms, node_uuid, gpu_index, gpu_uuid, gpu_model,
			util_sum, util_n, util_max, mem_used_sum, mem_used_n, mem_used_max, mem_total, power_sum, power_n, temp_sum, temp_n, temp_max)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (hour_ms, node_uuid, gpu_index, gpu_uuid) DO UPDATE SET
			gpu_model = CASE WHEN excluded.gpu_model <> '' THEN excluded.gpu_model ELSE gpu_model END,
			util_sum = util_sum + excluded.util_sum, util_n = util_n + excluded.util_n, util_max = MAX(util_max, excluded.util_max),
			mem_used_sum = mem_used_sum + excluded.mem_used_sum, mem_used_n = mem_used_n + excluded.mem_used_n,
			mem_used_max = MAX(mem_used_max, excluded.mem_used_max), mem_total = MAX(mem_total, excluded.mem_total),
			power_sum = power_sum + excluded.power_sum, power_n = power_n + excluded.power_n,
			temp_sum = temp_sum + excluded.temp_sum, temp_n = temp_n + excluded.temp_n, temp_max = MAX(temp_max, excluded.temp_max)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for k, g := range delta {
			card := k
			card.HourMS = 0
			if _, err := stmt.ExecContext(ctx, k.HourMS, k.NodeUUID, k.GPUIndex, k.GPUUUID, models[card],
				g.UtilSum, g.UtilN, g.UtilMax, g.MemUsedSum, g.MemUsedN, g.MemUsedMax, g.MemTotal,
				g.PowerSum, g.PowerN, g.TempSum, g.TempN, g.TempMax); err != nil {
				return err
			}
		}
		return nil
	})
}

// AddGPUTime adds the card time and capacity deltas in one transaction.
func (s *Store) AddGPUTime(ctx context.Context, times map[rollup.GPUTimeKey]rollup.GPUTime, capacity map[rollup.CapacityKey]rollup.Capacity) error {
	if len(times) == 0 && len(capacity) == 0 {
		return nil
	}
	return s.inTx(ctx, func(tx *sql.Tx) error {
		t, err := tx.PrepareContext(ctx, `INSERT INTO gpu_time_hourly (hour_ms, deployment_id, gpu_model,
			gpu_seconds, whole_gpu_seconds, instance_seconds, max_instances) VALUES (?, ?, ?, ?, ?, ?, ?)
			ON CONFLICT (hour_ms, deployment_id, gpu_model) DO UPDATE SET
			gpu_seconds = gpu_seconds + excluded.gpu_seconds, whole_gpu_seconds = whole_gpu_seconds + excluded.whole_gpu_seconds,
			instance_seconds = instance_seconds + excluded.instance_seconds, max_instances = MAX(max_instances, excluded.max_instances)`)
		if err != nil {
			return err
		}
		defer t.Close()
		for k, v := range times {
			if _, err := t.ExecContext(ctx, k.HourMS, k.DeploymentID, k.GPUModel, v.GPUSeconds, v.WholeGPUSeconds, v.InstanceSeconds, v.MaxInstances); err != nil {
				return err
			}
		}
		c, err := tx.PrepareContext(ctx, `INSERT INTO gpu_capacity_hourly (hour_ms, gpu_model, total_gpu_seconds, schedulable_gpu_seconds)
			VALUES (?, ?, ?, ?) ON CONFLICT (hour_ms, gpu_model) DO UPDATE SET
			total_gpu_seconds = total_gpu_seconds + excluded.total_gpu_seconds,
			schedulable_gpu_seconds = schedulable_gpu_seconds + excluded.schedulable_gpu_seconds`)
		if err != nil {
			return err
		}
		defer c.Close()
		for k, v := range capacity {
			if _, err := c.ExecContext(ctx, k.HourMS, k.GPUModel, v.TotalGPUSeconds, v.SchedulableGPUSeconds); err != nil {
				return err
			}
		}
		return nil
	})
}

func (s *Store) inTx(ctx context.Context, fn func(*sql.Tx) error) error {
	tx, err := s.writer.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer func() { _ = tx.Rollback() }()
	if err := fn(tx); err != nil {
		return err
	}
	return tx.Commit()
}

// RollupDay writes the day rows of day from its hour rows, replacing any
// written before, and records the day as rolled; running it again gives the
// same rows.
func (s *Store) RollupDay(ctx context.Context, day rollup.Day, loc *time.Location) error {
	from, to := day.HoursOf(loc)
	return s.inTx(ctx, func(tx *sql.Tx) error {
		for _, stmt := range []string{
			`DELETE FROM gpu_daily WHERE day = ?1`,
			`INSERT INTO gpu_daily SELECT ?1, node_uuid, gpu_index, gpu_uuid, MAX(gpu_model),
				SUM(util_sum), SUM(util_n), MAX(util_max), SUM(mem_used_sum), SUM(mem_used_n), MAX(mem_used_max), MAX(mem_total),
				SUM(power_sum), SUM(power_n), SUM(temp_sum), SUM(temp_n), MAX(temp_max)
				FROM gpu_hourly WHERE hour_ms >= ?2 AND hour_ms < ?3 GROUP BY node_uuid, gpu_index, gpu_uuid`,
			`DELETE FROM gpu_time_daily WHERE day = ?1`,
			`INSERT INTO gpu_time_daily SELECT ?1, deployment_id, gpu_model,
				SUM(gpu_seconds), SUM(whole_gpu_seconds), SUM(instance_seconds), MAX(max_instances)
				FROM gpu_time_hourly WHERE hour_ms >= ?2 AND hour_ms < ?3 GROUP BY deployment_id, gpu_model`,
			`DELETE FROM gpu_capacity_daily WHERE day = ?1`,
			`INSERT INTO gpu_capacity_daily SELECT ?1, gpu_model, SUM(total_gpu_seconds), SUM(schedulable_gpu_seconds)
				FROM gpu_capacity_hourly WHERE hour_ms >= ?2 AND hour_ms < ?3 GROUP BY gpu_model`,
			`DELETE FROM quality_daily WHERE day = ?1`,
		} {
			if _, err := tx.ExecContext(ctx, stmt, int64(day), from, to); err != nil {
				return fmt.Errorf("roll up %d: %w", day, err)
			}
		}
		days, err := rollQualityDay(ctx, tx, from, to)
		if err != nil {
			return err
		}
		stmt, err := tx.PrepareContext(ctx, `INSERT INTO quality_daily (day, deployment_id,
			requests, completed, length, client_closed, interrupted, error, ttft_hist, e2e_hist, output_tokens, decode_ms)
			VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`)
		if err != nil {
			return err
		}
		defer stmt.Close()
		for dep, q := range days {
			if _, err := stmt.ExecContext(ctx, append([]any{int64(day), dep}, qualityArgs(q)...)...); err != nil {
				return err
			}
		}
		_, err = tx.ExecContext(ctx, `INSERT OR IGNORE INTO rolled_days (day) VALUES (?)`, int64(day))
		return err
	})
}

func rollQualityDay(ctx context.Context, tx *sql.Tx, from, to int64) (map[uint]rollup.Quality, error) {
	rows, err := tx.QueryContext(ctx, `SELECT deployment_id, requests, completed, length, client_closed, interrupted, error,
		ttft_hist, e2e_hist, output_tokens, decode_ms FROM quality_hourly WHERE hour_ms >= ? AND hour_ms < ?`, from, to)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	out := map[uint]rollup.Quality{}
	for rows.Next() {
		var dep uint
		q, err := scanQuality(scanPrefixed{rows, &dep})
		if err != nil {
			return nil, err
		}
		cur := out[dep]
		cur.Add(q)
		out[dep] = cur
	}
	return out, rows.Err()
}

// scanPrefixed scans a leading column into first, the rest as the wrapped
// scanner's caller asks.
type scanPrefixed struct {
	rows  *sql.Rows
	first any
}

func (p scanPrefixed) Scan(dest ...any) error {
	return p.rows.Scan(append([]any{p.first}, dest...)...)
}

// RolledDay reports whether day's rows are rolled up.
func (s *Store) RolledDay(ctx context.Context, day rollup.Day) (bool, error) {
	var n int
	err := s.reader.QueryRowContext(ctx, `SELECT COUNT(*) FROM rolled_days WHERE day = ?`, int64(day)).Scan(&n)
	return n > 0, err
}

// EarliestHourMS is the oldest hour any hour table holds, or false when they
// are empty.
func (s *Store) EarliestHourMS(ctx context.Context) (int64, bool, error) {
	var v sql.NullInt64
	err := s.reader.QueryRowContext(ctx, `SELECT MIN(h) FROM (
		SELECT MIN(hour_ms) AS h FROM quality_hourly UNION ALL SELECT MIN(hour_ms) FROM gpu_hourly
		UNION ALL SELECT MIN(hour_ms) FROM gpu_time_hourly UNION ALL SELECT MIN(hour_ms) FROM gpu_capacity_hourly)`).Scan(&v)
	return v.Int64, v.Valid, err
}

// retainBatch bounds one delete statement, so retention never holds the
// writer long.
const retainBatch = 5000

// Retain deletes hour rows older than hourCutoffMS and day rows before
// dayCutoff, in batches, honouring ctx between them. It returns the rows
// deleted.
func (s *Store) Retain(ctx context.Context, hourCutoffMS int64, dayCutoff rollup.Day) (int64, error) {
	var total int64
	for _, step := range []struct {
		table, column string
		cutoff        int64
	}{
		{"quality_hourly", "hour_ms", hourCutoffMS}, {"gpu_hourly", "hour_ms", hourCutoffMS},
		{"gpu_time_hourly", "hour_ms", hourCutoffMS}, {"gpu_capacity_hourly", "hour_ms", hourCutoffMS},
		{"quality_daily", "day", int64(dayCutoff)}, {"gpu_daily", "day", int64(dayCutoff)},
		{"gpu_time_daily", "day", int64(dayCutoff)}, {"gpu_capacity_daily", "day", int64(dayCutoff)},
		{"rolled_days", "day", int64(dayCutoff)},
	} {
		for {
			if err := ctx.Err(); err != nil {
				return total, err
			}
			res, err := s.writer.ExecContext(ctx, `DELETE FROM `+step.table+` WHERE rowid IN (SELECT rowid FROM `+
				step.table+` WHERE `+step.column+` < ? LIMIT ?)`, step.cutoff, retainBatch)
			if err != nil {
				return total, fmt.Errorf("retain %s: %w", step.table, err)
			}
			n, _ := res.RowsAffected()
			total += n
			if n < retainBatch {
				break
			}
		}
	}
	return total, nil
}

// Backfill is how far the one-off fill of a kind got.
type Backfill struct {
	ThroughMS int64
	Done      bool
}

// BackfillState reads a kind's fill; a kind never started reads as zero.
func (s *Store) BackfillState(ctx context.Context, kind Kind) (Backfill, bool, error) {
	var b Backfill
	err := s.reader.QueryRowContext(ctx, `SELECT through_ms, done FROM backfill WHERE kind = ?`, string(kind)).Scan(&b.ThroughMS, &b.Done)
	if err == sql.ErrNoRows {
		return Backfill{}, false, nil
	}
	return b, err == nil, err
}

// SetBackfillState records a kind's fill.
func (s *Store) SetBackfillState(ctx context.Context, kind Kind, b Backfill) error {
	_, err := s.writer.ExecContext(ctx, `INSERT INTO backfill (kind, through_ms, done) VALUES (?, ?, ?)
		ON CONFLICT (kind) DO UPDATE SET through_ms = excluded.through_ms, done = excluded.done`, string(kind), b.ThroughMS, b.Done)
	return err
}
