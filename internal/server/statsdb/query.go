package statsdb

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/rollup"
)

// Reading the statistics back for operations analytics. A range is over hour
// rows (bucket = hour_ms) or day rows (bucket = YYYYMMDD); every query starts
// on the primary key's leading column and aggregates in SQL where it can.

// Range is a span of hour or day rows: [From, To) of hour_ms, or of days.
type Range struct {
	Daily    bool
	From, To int64
}

func (r Range) table(base string) (table, column string) {
	if r.Daily {
		return base + "_daily", "day"
	}
	return base + "_hourly", "hour_ms"
}

// QualityRow is one bucket of a deployment's quality.
type QualityRow struct {
	Bucket       int64
	DeploymentID uint
	rollup.Quality
}

// Quality reads the quality rows in range, of one deployment when
// deploymentID is not 0. A whole fleet's week of hours is 84 000 rows of a
// few dozen bytes; the histograms decode in Go.
func (s *Store) Quality(ctx context.Context, r Range, deploymentID uint) ([]QualityRow, error) {
	table, column := r.table("quality")
	q := `SELECT ` + column + `, deployment_id, requests, completed, length, client_closed, interrupted, error,
		ttft_hist, e2e_hist, output_tokens, decode_ms FROM ` + table + ` WHERE ` + column + ` >= ? AND ` + column + ` < ?`
	args := []any{r.From, r.To}
	if deploymentID != 0 {
		q += ` AND deployment_id = ?`
		args = append(args, deploymentID)
	}
	rows, err := s.reader.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []QualityRow
	for rows.Next() {
		var row QualityRow
		var ttft, e2e []byte
		if err := rows.Scan(&row.Bucket, &row.DeploymentID, &row.Requests, &row.Completed, &row.Length, &row.ClientClosed,
			&row.Interrupted, &row.Error, &ttft, &e2e, &row.OutputTokens, &row.DecodeMS); err != nil {
			return nil, err
		}
		if row.TTFT, err = rollup.DecodeHistogram(ttft); err != nil {
			return nil, err
		}
		if row.EndToEnd, err = rollup.DecodeHistogram(e2e); err != nil {
			return nil, err
		}
		out = append(out, row)
	}
	return out, rows.Err()
}

// GPUFilter narrows the GPU queries; empty values do not filter.
type GPUFilter struct {
	NodeUUID string
	GPUModel string
}

func (f GPUFilter) where() (string, []any) {
	var clauses []string
	var args []any
	if f.NodeUUID != "" {
		clauses = append(clauses, "node_uuid = ?")
		args = append(args, f.NodeUUID)
	}
	if f.GPUModel != "" {
		clauses = append(clauses, "gpu_model = ?")
		args = append(args, f.GPUModel)
	}
	if len(clauses) == 0 {
		return "", nil
	}
	return " AND " + strings.Join(clauses, " AND "), args
}

// GPUCard is a card's statistics summed over a range.
type GPUCard struct {
	NodeUUID string
	GPUIndex int
	GPUUUID  string
	GPUModel string
	rollup.GPU
}

const gpuSums = `SUM(util_sum), SUM(util_n), MAX(util_max), SUM(mem_used_sum), SUM(mem_used_n), MAX(mem_used_max), MAX(mem_total),
	SUM(power_sum), SUM(power_n), SUM(temp_sum), SUM(temp_n), MAX(temp_max)`

func scanGPU(dest *rollup.GPU) []any {
	return []any{&dest.UtilSum, &dest.UtilN, &dest.UtilMax, &dest.MemUsedSum, &dest.MemUsedN, &dest.MemUsedMax, &dest.MemTotal,
		&dest.PowerSum, &dest.PowerN, &dest.TempSum, &dest.TempN, &dest.TempMax}
}

// GPUCards sums every card over a range.
func (s *Store) GPUCards(ctx context.Context, r Range, f GPUFilter) ([]GPUCard, error) {
	table, column := r.table("gpu")
	where, args := f.where()
	rows, err := s.reader.QueryContext(ctx, `SELECT node_uuid, gpu_index, gpu_uuid, MAX(gpu_model), `+gpuSums+` FROM `+table+
		` WHERE `+column+` >= ? AND `+column+` < ?`+where+` GROUP BY node_uuid, gpu_index, gpu_uuid`, append([]any{r.From, r.To}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GPUCard
	for rows.Next() {
		var c GPUCard
		if err := rows.Scan(append([]any{&c.NodeUUID, &c.GPUIndex, &c.GPUUUID, &c.GPUModel}, scanGPU(&c.GPU)...)...); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GPUGroupBy is what a GPU heat map's rows are.
type GPUGroupBy string

const (
	GPUByModel GPUGroupBy = "model"
	GPUByNode  GPUGroupBy = "node"
	GPUByCard  GPUGroupBy = "gpu"
)

// GPUCell is one bucket of one heat map row: a model, a node, or a card
// (node_uuid/gpu_index/gpu_uuid joined by "/").
type GPUCell struct {
	Bucket int64
	Group  string
	rollup.GPU
}

// GPUHeat sums the cards of each group per bucket.
func (s *Store) GPUHeat(ctx context.Context, r Range, f GPUFilter, by GPUGroupBy) ([]GPUCell, error) {
	table, column := r.table("gpu")
	var group string
	switch by {
	case GPUByModel:
		group = "gpu_model"
	case GPUByNode:
		group = "node_uuid"
	case GPUByCard:
		group = "node_uuid || '/' || gpu_index || '/' || gpu_uuid"
	default:
		return nil, fmt.Errorf("statsdb: no GPU grouping %q", by)
	}
	where, args := f.where()
	rows, err := s.reader.QueryContext(ctx, `SELECT `+column+`, `+group+`, `+gpuSums+` FROM `+table+
		` WHERE `+column+` >= ? AND `+column+` < ?`+where+` GROUP BY `+column+`, `+group, append([]any{r.From, r.To}, args...)...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []GPUCell
	for rows.Next() {
		var c GPUCell
		if err := rows.Scan(append([]any{&c.Bucket, &c.Group}, scanGPU(&c.GPU)...)...); err != nil {
			return nil, err
		}
		out = append(out, c)
	}
	return out, rows.Err()
}

// GPUTimeRow is one bucket of a deployment's card time on a model.
type GPUTimeRow struct {
	Bucket       int64
	DeploymentID uint
	GPUModel     string
	rollup.GPUTime
}

// GPUTime reads the card time per bucket, deployment and model.
func (s *Store) GPUTime(ctx context.Context, r Range, deploymentID uint) ([]GPUTimeRow, error) {
	table, column := r.table("gpu_time")
	q := `SELECT ` + column + `, deployment_id, gpu_model, gpu_seconds, whole_gpu_seconds, instance_seconds, max_instances FROM ` +
		table + ` WHERE ` + column + ` >= ? AND ` + column + ` < ?`
	args := []any{r.From, r.To}
	if deploymentID != 0 {
		q += ` AND deployment_id = ?`
		args = append(args, deploymentID)
	}
	return scanRows(ctx, s.reader, q, args, func(rows *sql.Rows) (GPUTimeRow, error) {
		var t GPUTimeRow
		err := rows.Scan(&t.Bucket, &t.DeploymentID, &t.GPUModel, &t.GPUSeconds, &t.WholeGPUSeconds, &t.InstanceSeconds, &t.MaxInstances)
		return t, err
	})
}

// CapacityRow is one bucket of the cluster's cards of a model.
type CapacityRow struct {
	Bucket   int64
	GPUModel string
	rollup.Capacity
}

// Capacity reads the card capacity per bucket and model.
func (s *Store) Capacity(ctx context.Context, r Range) ([]CapacityRow, error) {
	table, column := r.table("gpu_capacity")
	return scanRows(ctx, s.reader, `SELECT `+column+`, gpu_model, total_gpu_seconds, schedulable_gpu_seconds FROM `+table+
		` WHERE `+column+` >= ? AND `+column+` < ?`, []any{r.From, r.To}, func(rows *sql.Rows) (CapacityRow, error) {
		var c CapacityRow
		err := rows.Scan(&c.Bucket, &c.GPUModel, &c.TotalGPUSeconds, &c.SchedulableGPUSeconds)
		return c, err
	})
}

// RolledDays are the days from fromDay up to toDay that are rolled up.
func (s *Store) RolledDays(ctx context.Context, fromDay, toDay rollup.Day) (map[rollup.Day]bool, error) {
	rows, err := scanRows(ctx, s.reader, `SELECT day FROM rolled_days WHERE day >= ? AND day < ?`, []any{int64(fromDay), int64(toDay)},
		func(rows *sql.Rows) (rollup.Day, error) {
			var d int64
			err := rows.Scan(&d)
			return rollup.Day(d), err
		})
	out := make(map[rollup.Day]bool, len(rows))
	for _, d := range rows {
		out[d] = true
	}
	return out, err
}

func scanRows[T any](ctx context.Context, db *sql.DB, q string, args []any, scan func(*sql.Rows) (T, error)) ([]T, error) {
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var out []T
	for rows.Next() {
		v, err := scan(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, v)
	}
	return out, rows.Err()
}
