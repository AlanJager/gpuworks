package statsdb

import (
	"context"
	"database/sql"
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/rollup"
)

// Reading the GPU statistics back. A fleet's pages read the node and model
// rows (a node's cards summed, a model's cards summed); a card's own rows are
// read for a few cards at a time. A query narrowed to some nodes names their
// buckets and nodes outright, so SQLite seeks each (bucket, node) on the
// primary key instead of reading every node's rows of the range.

// GPUFilter narrows the GPU queries.
type GPUFilter struct {
	// NodeUUIDs, when not nil, are the only nodes read: an empty list reads
	// none.
	NodeUUIDs []string
	// GPUModel, when not empty, is the only card model read.
	GPUModel string
}

// GPUGroupBy is what a GPU query's rows are.
type GPUGroupBy string

const (
	GPUByModel GPUGroupBy = "model"
	GPUByNode  GPUGroupBy = "node"
	GPUByCard  GPUGroupBy = "gpu"
)

const gpuSums = `SUM(util_sum), SUM(util_n), MAX(util_max), SUM(mem_used_sum), SUM(mem_used_n), MAX(mem_used_max), MAX(mem_total),
	SUM(power_sum), SUM(power_n), SUM(temp_sum), SUM(temp_n), MAX(temp_max)`

func scanGPU(dest *rollup.GPU) []any {
	return []any{&dest.UtilSum, &dest.UtilN, &dest.UtilMax, &dest.MemUsedSum, &dest.MemUsedN, &dest.MemUsedMax, &dest.MemTotal,
		&dest.PowerSum, &dest.PowerN, &dest.TempSum, &dest.TempN, &dest.TempMax}
}

// keys are the bucket keys of a range: its hours, or its days.
func (r Range) keys() []any {
	var out []any
	if r.Daily {
		for d := rollup.Day(r.From); int64(d) < r.To; d = d.Next() {
			out = append(out, int64(d))
		}
		return out
	}
	for h := r.From; h < r.To; h += hourSpanMS {
		out = append(out, h)
	}
	return out
}

const hourSpanMS = int64(3_600_000)

// gpuWhere is the WHERE clause of a GPU table's range under f: the bucket
// range, or with nodes named each bucket and node, for the primary key to
// seek.
func gpuWhere(r Range, column string, f GPUFilter, nodes []string) (string, []any) {
	var clauses []string
	var args []any
	if nodes == nil {
		clauses = append(clauses, column+" >= ? AND "+column+" < ?")
		args = append(args, r.From, r.To)
	} else {
		keys := r.keys()
		clauses = append(clauses, column+" IN ("+marks(len(keys))+")", "node_uuid IN ("+marks(len(nodes))+")")
		args = append(args, keys...)
		for _, n := range nodes {
			args = append(args, n)
		}
	}
	if f.GPUModel != "" {
		clauses = append(clauses, "gpu_model = ?")
		args = append(args, f.GPUModel)
	}
	return " WHERE " + strings.Join(clauses, " AND "), args
}

func marks(n int) string {
	return strings.TrimSuffix(strings.Repeat("?, ", n), ", ")
}

// GPUCard is a card's statistics summed over a range.
type GPUCard struct {
	NodeUUID string
	GPUIndex int
	GPUUUID  string
	GPUModel string
	rollup.GPU
}

// GPUCards sums each card over a range, from the cards' own rows: for a few
// cards, so f should name their nodes.
func (s *Store) GPUCards(ctx context.Context, r Range, f GPUFilter) ([]GPUCard, error) {
	if f.NodeUUIDs != nil && len(f.NodeUUIDs) == 0 {
		return nil, nil
	}
	table, column := r.table("gpu")
	where, args := gpuWhere(r, column, f, f.NodeUUIDs)
	return scanRows(ctx, s.reader, `SELECT node_uuid, gpu_index, gpu_uuid, MAX(gpu_model), `+gpuSums+` FROM `+table+where+
		` GROUP BY node_uuid, gpu_index, gpu_uuid`, args, func(rows *sql.Rows) (GPUCard, error) {
		var c GPUCard
		err := rows.Scan(append([]any{&c.NodeUUID, &c.GPUIndex, &c.GPUUUID, &c.GPUModel}, scanGPU(&c.GPU)...)...)
		return c, err
	})
}

// GPUGroup is a group's statistics summed over a range: a model, a node, or
// a card (node_uuid/gpu_index/gpu_uuid joined by "/").
type GPUGroup struct {
	Group string
	rollup.GPU
}

// GPUCell is one bucket of a group.
type GPUCell struct {
	Bucket int64
	Group  string
	rollup.GPU
}

// gpuSource is the table a grouping reads under f and its group column: the
// model rows for the models of every node, the node rows otherwise, the
// cards' own rows by card.
func gpuSource(r Range, f GPUFilter, by GPUGroupBy) (table, column, group string, err error) {
	switch by {
	case GPUByModel:
		if f.NodeUUIDs == nil {
			table, column = r.table("gpu_model")
			return table, column, "gpu_model", nil
		}
		table, column = r.table("gpu_node")
		return table, column, "gpu_model", nil
	case GPUByNode:
		table, column = r.table("gpu_node")
		return table, column, "node_uuid", nil
	case GPUByCard:
		table, column = r.table("gpu")
		return table, column, "node_uuid || '/' || gpu_index || '/' || gpu_uuid", nil
	}
	return "", "", "", fmt.Errorf("statsdb: no GPU grouping %q", by)
}

// GPUGroups sums each group over a range.
func (s *Store) GPUGroups(ctx context.Context, r Range, f GPUFilter, by GPUGroupBy) ([]GPUGroup, error) {
	if f.NodeUUIDs != nil && len(f.NodeUUIDs) == 0 {
		return nil, nil
	}
	table, column, group, err := gpuSource(r, f, by)
	if err != nil {
		return nil, err
	}
	where, args := gpuWhere(r, column, f, f.NodeUUIDs)
	return scanRows(ctx, s.reader, `SELECT `+group+`, `+gpuSums+` FROM `+table+where+` GROUP BY `+group, args,
		func(rows *sql.Rows) (GPUGroup, error) {
			var g GPUGroup
			err := rows.Scan(append([]any{&g.Group}, scanGPU(&g.GPU)...)...)
			return g, err
		})
}

// GPUHeat sums each group per bucket. Grouped by node, only nodes are read
// (nil reads every node f allows): a heat map shows a few rows of a fleet.
func (s *Store) GPUHeat(ctx context.Context, r Range, f GPUFilter, by GPUGroupBy, nodes []string) ([]GPUCell, error) {
	if nodes == nil {
		nodes = f.NodeUUIDs
	}
	if nodes != nil && len(nodes) == 0 {
		return nil, nil
	}
	table, column, group, err := gpuSource(r, f, by)
	if err != nil {
		return nil, err
	}
	where, args := gpuWhere(r, column, f, nodes)
	return scanRows(ctx, s.reader, `SELECT `+column+`, `+group+`, `+gpuSums+` FROM `+table+where+` GROUP BY `+column+`, `+group, args,
		func(rows *sql.Rows) (GPUCell, error) {
			var c GPUCell
			err := rows.Scan(append([]any{&c.Bucket, &c.Group}, scanGPU(&c.GPU)...)...)
			return c, err
		})
}
