package statsdb

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/rollup"
)

func TestGPUNodeAndModelRowsSumTheCards(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	h := hourMS(21, 16)
	card := func(node string, index int) rollup.GPUKey {
		return rollup.GPUKey{HourMS: h, NodeUUID: node, GPUIndex: index, GPUUUID: fmt.Sprintf("GPU-%s-%d", node, index)}
	}
	named := func(k rollup.GPUKey) rollup.GPUKey { k.HourMS = 0; return k }
	g := func(util, memTotal float64) rollup.GPU {
		return rollup.GPU{UtilSum: util * 2, UtilN: 2, UtilMax: util, MemUsedSum: 2 * 40e9, MemUsedN: 2, MemUsedMax: 41e9, MemTotal: memTotal,
			PowerSum: 600, PowerN: 2, TempSum: 120, TempN: 2, TempMax: 61}
	}
	// n1 holds an H100 and an L40S, n2 an H100.
	a, b, c := card("n1", 0), card("n1", 1), card("n2", 0)
	models := map[rollup.GPUKey]string{named(a): "H100", named(b): "L40S", named(c): "H100"}
	if err := s.AddGPUs(ctx, map[rollup.GPUKey]rollup.GPU{a: g(80, 80e9), b: g(20, 48e9), c: g(50, 80e9)}, models); err != nil {
		t.Fatal(err)
	}
	// A later delta that names no model keeps each card's.
	if err := s.AddGPUs(ctx, map[rollup.GPUKey]rollup.GPU{a: g(80, 80e9), c: g(50, 80e9)}, nil); err != nil {
		t.Fatal(err)
	}
	type row struct {
		key              string
		utilSum, utilMax float64
		utilN            int64
		memTotal         float64
	}
	read := func(q string) []row {
		t.Helper()
		rows, err := s.reader.Query(q)
		if err != nil {
			t.Fatal(err)
		}
		defer rows.Close()
		var out []row
		for rows.Next() {
			var r row
			if err := rows.Scan(&r.key, &r.utilSum, &r.utilN, &r.utilMax, &r.memTotal); err != nil {
				t.Fatal(err)
			}
			out = append(out, r)
		}
		return out
	}
	nodes := read(`SELECT node_uuid || '/' || gpu_model, util_sum, util_n, util_max, mem_total FROM gpu_node_hourly ORDER BY 1`)
	wantNodes := []row{{key: "n1/H100", utilSum: 320, utilN: 4, utilMax: 80, memTotal: 80e9}, {key: "n1/L40S", utilSum: 40, utilN: 2, utilMax: 20, memTotal: 48e9}, {key: "n2/H100", utilSum: 200, utilN: 4, utilMax: 50, memTotal: 80e9}}
	if fmt.Sprint(nodes) != fmt.Sprint(wantNodes) {
		t.Fatalf("node rows = %v, want %v", nodes, wantNodes)
	}
	byModel := read(`SELECT gpu_model, util_sum, util_n, util_max, mem_total FROM gpu_model_hourly ORDER BY 1`)
	wantModels := []row{{key: "H100", utilSum: 520, utilN: 8, utilMax: 80, memTotal: 80e9}, {key: "L40S", utilSum: 40, utilN: 2, utilMax: 20, memTotal: 48e9}}
	if fmt.Sprint(byModel) != fmt.Sprint(wantModels) {
		t.Fatalf("model rows = %v, want %v", byModel, wantModels)
	}
	// Every node and model row is the sum of its cards' rows.
	var cards, perNode, perModel float64
	for q, dest := range map[string]*float64{
		`SELECT SUM(util_sum) FROM gpu_hourly`: &cards, `SELECT SUM(util_sum) FROM gpu_node_hourly`: &perNode, `SELECT SUM(util_sum) FROM gpu_model_hourly`: &perModel,
	} {
		if err := s.reader.QueryRow(q).Scan(dest); err != nil {
			t.Fatal(err)
		}
	}
	if cards != perNode || cards != perModel {
		t.Fatalf("util sums: cards %v, nodes %v, models %v", cards, perNode, perModel)
	}

	shanghai, _ := time.LoadLocation("Asia/Shanghai")
	for range 2 {
		if err := s.RollupDay(ctx, 20261022, shanghai); err != nil {
			t.Fatal(err)
		}
	}
	nodeDays := read(`SELECT node_uuid || '/' || gpu_model, util_sum, util_n, util_max, mem_total FROM gpu_node_daily ORDER BY 1`)
	if fmt.Sprint(nodeDays) != fmt.Sprint(wantNodes) {
		t.Fatalf("node days = %v, want %v", nodeDays, wantNodes)
	}
	modelDays := read(`SELECT gpu_model, util_sum, util_n, util_max, mem_total FROM gpu_model_daily ORDER BY 1`)
	if fmt.Sprint(modelDays) != fmt.Sprint(wantModels) {
		t.Fatalf("model days = %v, want %v", modelDays, wantModels)
	}
	hourly, daily, err := s.RowCount(ctx, KindGPU)
	if err != nil || hourly != 3+3+2 || daily != 3+3+2 {
		t.Fatalf("gpu rows = %d %d %v", hourly, daily, err)
	}
}

// writeV1 makes a stats.db as the first schema left it, holding two cards'
// hour and day rows; extra statements run after.
func writeV1(t *testing.T, extra ...string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stats.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	figures := `1.5, 1, 1.5, 2, 1, 2, 80, 300, 1, 60, 1, 60`
	stmts := append(append([]string{}, schemaV1...),
		`INSERT INTO gpu_hourly VALUES (1000, 'n1', 0, 'GPU-a', 'H100', `+figures+`)`,
		`INSERT INTO gpu_hourly VALUES (1000, 'n1', 1, 'GPU-b', 'H100', `+figures+`)`,
		`INSERT INTO gpu_daily VALUES (20261021, 'n1', 0, 'GPU-a', 'H100', `+figures+`)`,
		`PRAGMA user_version = 1`)
	for _, stmt := range append(stmts, extra...) {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatalf("%s: %v", stmt, err)
		}
	}
	return path
}

func TestSchemaV2FillsTheNodeAndModelRowsOfAFirstSchemaFile(t *testing.T) {
	path := writeV1(t)
	for range 2 { // the second open finds the current version and changes nothing
		s, err := Open(path)
		if err != nil {
			t.Fatal(err)
		}
		var version int
		var nodeUtil, modelUtil, nodeDay float64
		var nodeN int64
		if err := s.reader.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 2 {
			t.Fatalf("version = %d %v", version, err)
		}
		if err := s.reader.QueryRow(`SELECT util_sum, util_n FROM gpu_node_hourly WHERE hour_ms = 1000 AND node_uuid = 'n1' AND gpu_model = 'H100'`).Scan(&nodeUtil, &nodeN); err != nil || nodeUtil != 3 || nodeN != 2 {
			t.Fatalf("node hour = %v %v %v", nodeUtil, nodeN, err)
		}
		if err := s.reader.QueryRow(`SELECT util_sum FROM gpu_model_hourly WHERE hour_ms = 1000 AND gpu_model = 'H100'`).Scan(&modelUtil); err != nil || modelUtil != 3 {
			t.Fatalf("model hour = %v %v", modelUtil, err)
		}
		if err := s.reader.QueryRow(`SELECT util_sum FROM gpu_node_daily WHERE day = 20261021`).Scan(&nodeDay); err != nil || nodeDay != 1.5 {
			t.Fatalf("node day = %v %v", nodeDay, err)
		}
		hourly, daily, err := s.RowCount(context.Background(), KindGPU)
		if err != nil || hourly != 2+1+1 || daily != 1+1+1 {
			t.Fatalf("gpu rows = %d %d %v", hourly, daily, err)
		}
		_ = s.Close()
	}
}

func TestSchemaV2FailingHalfwayLeavesTheFirstSchema(t *testing.T) {
	// A table in the way of the step's third statement fails it after two
	// tables are made: the step and the version roll back together.
	path := writeV1(t, `CREATE TABLE gpu_model_hourly (x INTEGER)`)
	if _, err := Open(path); err == nil {
		t.Fatal("the step succeeded over a table in its way")
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var version, made int
	if err := db.QueryRow(`PRAGMA user_version`).Scan(&version); err != nil || version != 1 {
		t.Fatalf("version = %d %v", version, err)
	}
	if err := db.QueryRow(`SELECT COUNT(*) FROM sqlite_master WHERE name IN ('gpu_node_hourly', 'gpu_node_daily')`).Scan(&made); err != nil || made != 0 {
		t.Fatalf("tables left by the failed step = %d %v", made, err)
	}
	if _, err := db.Exec(`DROP TABLE gpu_model_hourly`); err != nil {
		t.Fatal(err)
	}
	// With the obstacle gone the step runs on the next start.
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	_ = s.Close()
}

// Named nodes are sought bucket by bucket on the primary key: both of its
// leading columns are constrained, not just the first.
func TestGPUQueriesNamingNodesSeekBothKeyColumns(t *testing.T) {
	s, _ := openTest(t)
	r := Range{From: 0, To: 3 * hourSpanMS}
	for _, table := range []string{"gpu_hourly", "gpu_node_hourly"} {
		where, args := gpuWhere(r, "hour_ms", GPUFilter{}, []string{"a", "b"})
		rows, err := s.reader.Query(`EXPLAIN QUERY PLAN SELECT SUM(util_sum) FROM `+table+where, args...)
		if err != nil {
			t.Fatal(err)
		}
		var plan []string
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
				t.Fatal(err)
			}
			plan = append(plan, detail)
		}
		rows.Close()
		if !strings.Contains(strings.Join(plan, "\n"), "hour_ms=? AND node_uuid=?") {
			t.Fatalf("%s plan: %v", table, plan)
		}
	}
	if keys := (Range{Daily: true, From: 20261030, To: 20261102}).keys(); fmt.Sprint(keys) != "[20261030 20261031 20261101]" {
		t.Fatalf("day keys = %v", keys)
	}
	if keys := r.keys(); len(keys) != 3 {
		t.Fatalf("hour keys = %v", keys)
	}
}
