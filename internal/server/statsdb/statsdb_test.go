package statsdb

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/rollup"
)

func openTest(t *testing.T) (*Store, string) {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stats.db")
	s, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s, path
}

func hourMS(day, hour int) int64 {
	return time.Date(2026, 10, day, hour, 0, 0, 0, time.UTC).UnixMilli()
}

func TestStatsAddRollUpAndRetain(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	shanghai, _ := time.LoadLocation("Asia/Shanghai")

	var q rollup.Quality
	q.Requests, q.Completed, q.Error = 10, 9, 1
	for i := 0; i < 9; i++ {
		q.TTFT.Observe(300)
		q.EndToEnd.Observe(5000)
	}
	q.OutputTokens, q.DecodeMS = 900, 9000
	qk := rollup.QualityKey{HourMS: hourMS(21, 16), DeploymentID: 17}
	for i := 0; i < 2; i++ {
		if err := s.AddQuality(ctx, map[rollup.QualityKey]rollup.Quality{qk: q}); err != nil {
			t.Fatal(err)
		}
	}
	other := rollup.QualityKey{HourMS: hourMS(22, 2), DeploymentID: 17}
	if err := s.AddQuality(ctx, map[rollup.QualityKey]rollup.Quality{other: q}); err != nil {
		t.Fatal(err)
	}
	got, err := scanQuality(s.reader.QueryRow(`SELECT requests, completed, length, client_closed, interrupted, error,
		ttft_hist, e2e_hist, output_tokens, decode_ms FROM quality_hourly WHERE hour_ms = ? AND deployment_id = 17`, qk.HourMS))
	if err != nil || got.Requests != 20 || got.TTFT.Count() != 18 || got.OutputTokens != 1800 {
		t.Fatalf("merged quality = %+v %v", got, err)
	}

	gk := rollup.GPUKey{HourMS: hourMS(21, 16), NodeUUID: "n1", GPUIndex: 0, GPUUUID: "GPU-1"}
	g := rollup.GPU{UtilSum: 360, UtilN: 6, UtilMax: 90, MemUsedSum: 6 * 70e9, MemUsedN: 6, MemUsedMax: 71e9, MemTotal: 84e9, PowerSum: 1800, PowerN: 6, TempSum: 360, TempN: 6, TempMax: 66}
	for i := 0; i < 2; i++ {
		if err := s.AddGPUs(ctx, map[rollup.GPUKey]rollup.GPU{gk: g}, map[rollup.GPUKey]string{{NodeUUID: "n1", GPUIndex: 0, GPUUUID: "GPU-1"}: "RTX 6000D"}); err != nil {
			t.Fatal(err)
		}
	}
	tk := rollup.GPUTimeKey{HourMS: hourMS(21, 16), DeploymentID: 17, GPUModel: "RTX 6000D"}
	ck := rollup.CapacityKey{HourMS: hourMS(21, 16), GPUModel: "RTX 6000D"}
	for _, n := range []int64{2, 1} {
		if err := s.AddGPUTime(ctx, map[rollup.GPUTimeKey]rollup.GPUTime{tk: {GPUSeconds: 240, WholeGPUSeconds: 240, InstanceSeconds: 60, MaxInstances: n}},
			map[rollup.CapacityKey]rollup.Capacity{ck: {TotalGPUSeconds: 240, SchedulableGPUSeconds: 240}}); err != nil {
			t.Fatal(err)
		}
	}
	var util, maxInst, capacity float64
	if err := s.reader.QueryRow(`SELECT util_sum FROM gpu_hourly`).Scan(&util); err != nil || util != 720 {
		t.Fatalf("gpu util = %v %v", util, err)
	}
	if err := s.reader.QueryRow(`SELECT max_instances FROM gpu_time_hourly`).Scan(&maxInst); err != nil || maxInst != 2 {
		t.Fatalf("max instances = %v %v", maxInst, err)
	}
	if err := s.reader.QueryRow(`SELECT total_gpu_seconds FROM gpu_capacity_hourly`).Scan(&capacity); err != nil || capacity != 480 {
		t.Fatalf("capacity = %v %v", capacity, err)
	}

	for i := 0; i < 2; i++ {
		if err := s.RollupDay(ctx, 20261022, shanghai); err != nil {
			t.Fatal(err)
		}
	}
	var dayReq, dayUtilMax, dayGPUSeconds, dayCap float64
	var ttft []byte
	if err := s.reader.QueryRow(`SELECT requests, ttft_hist FROM quality_daily WHERE day = 20261022 AND deployment_id = 17`).Scan(&dayReq, &ttft); err != nil || dayReq != 30 {
		t.Fatalf("day quality = %v %v", dayReq, err)
	}
	if h, _ := rollup.DecodeHistogram(ttft); h.Count() != 27 {
		t.Fatalf("day ttft = %d", h.Count())
	}
	var model string
	if err := s.reader.QueryRow(`SELECT util_max, gpu_model FROM gpu_daily`).Scan(&dayUtilMax, &model); err != nil || dayUtilMax != 90 || model != "RTX 6000D" {
		t.Fatalf("day util max = %v %v", dayUtilMax, err)
	}
	if err := s.reader.QueryRow(`SELECT gpu_seconds FROM gpu_time_daily`).Scan(&dayGPUSeconds); err != nil || dayGPUSeconds != 480 {
		t.Fatalf("day gpu seconds = %v %v", dayGPUSeconds, err)
	}
	if err := s.reader.QueryRow(`SELECT schedulable_gpu_seconds FROM gpu_capacity_daily`).Scan(&dayCap); err != nil || dayCap != 480 {
		t.Fatalf("day capacity = %v %v", dayCap, err)
	}
	if rolled, err := s.RolledDay(ctx, 20261022); err != nil || !rolled {
		t.Fatal("not rolled")
	}
	if earliest, ok, err := s.EarliestHourMS(ctx); err != nil || !ok || earliest != hourMS(21, 16) {
		t.Fatalf("earliest = %d %v %v", earliest, ok, err)
	}
	hourly, daily, err := s.RowCount(ctx, KindQuality)
	if err != nil || hourly != 2 || daily != 1 {
		t.Fatalf("quality rows = %d %d", hourly, daily)
	}
	n, err := s.Retain(ctx, hourMS(22, 0), 20261023)
	// An hour row and a day row of each table (the GPU's per card, node and
	// model) and the rolled day; quality's second hour is kept.
	if err != nil || n != 1+3+1+1+1+3+1+1+1 {
		t.Fatalf("retained %d %v", n, err)
	}
	hourly, daily, _ = s.RowCount(ctx, KindQuality)
	if hourly != 1 || daily != 0 {
		t.Fatalf("after retention = %d %d", hourly, daily)
	}
	if s.SizeBytes() <= 0 {
		t.Fatal("no size")
	}
}

func TestStatsBackfillStateAndSchemaVersion(t *testing.T) {
	s, path := openTest(t)
	ctx := context.Background()
	if _, ok, err := s.BackfillState(ctx, KindQuality); ok || err != nil {
		t.Fatalf("fresh state = %v %v", ok, err)
	}
	if err := s.SetBackfillState(ctx, KindQuality, Backfill{ThroughMS: 5}); err != nil {
		t.Fatal(err)
	}
	if err := s.SetBackfillState(ctx, KindQuality, Backfill{ThroughMS: 7, Done: true}); err != nil {
		t.Fatal(err)
	}
	if b, ok, err := s.BackfillState(ctx, KindQuality); !ok || err != nil || b.ThroughMS != 7 || !b.Done {
		t.Fatalf("state = %+v %v %v", b, ok, err)
	}
	_ = s.Close()
	// Reopening a current file keeps its rows.
	again, err := Open(path)
	if err != nil {
		t.Fatal(err)
	}
	if b, ok, _ := again.BackfillState(ctx, KindQuality); !ok || b.ThroughMS != 7 {
		t.Fatal("reopen lost the state")
	}
	_ = again.Close()
	// A file from a newer release is refused, not misread.
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(`PRAGMA user_version = 99`); err != nil {
		t.Fatal(err)
	}
	_ = db.Close()
	if _, err := Open(path); !errors.Is(err, ErrNewerSchema) {
		t.Fatalf("newer schema = %v", err)
	}
}

// Rolling a day up and ranging over hours read the primary key, never the
// whole table.
func TestStatsQueriesUseThePrimaryKey(t *testing.T) {
	s, _ := openTest(t)
	for _, q := range []string{
		`SELECT SUM(util_sum) FROM gpu_hourly WHERE hour_ms >= 0 AND hour_ms < 1 GROUP BY node_uuid, gpu_index, gpu_uuid`,
		`SELECT SUM(gpu_seconds) FROM gpu_time_hourly WHERE hour_ms >= 0 AND hour_ms < 1 GROUP BY deployment_id, gpu_model`,
		`SELECT requests FROM quality_hourly WHERE hour_ms >= 0 AND hour_ms < 1`,
		`SELECT requests FROM quality_daily WHERE day >= 0 AND day < 1`,
		`SELECT SUM(util_sum) FROM gpu_node_hourly WHERE hour_ms >= 0 AND hour_ms < 1 GROUP BY node_uuid`,
		`SELECT SUM(util_sum) FROM gpu_model_daily WHERE day >= 0 AND day < 1 GROUP BY gpu_model`,
	} {
		rows, err := s.reader.Query(`EXPLAIN QUERY PLAN ` + q)
		if err != nil {
			t.Fatal(err)
		}
		var details []string
		for rows.Next() {
			var id, parent, notused int
			var detail string
			if err := rows.Scan(&id, &parent, &notused, &detail); err != nil {
				t.Fatal(err)
			}
			details = append(details, detail)
		}
		rows.Close()
		ok := false
		for _, d := range details {
			if len(d) >= 6 && (strings.Contains(d, "USING INDEX") || strings.Contains(d, "USING PRIMARY KEY") || strings.Contains(d, "sqlite_autoindex")) {
				ok = true
			}
		}
		if !ok {
			t.Fatalf("%s\nplan: %v", q, details)
		}
	}
}

func TestStatsQueriesReadRanges(t *testing.T) {
	s, _ := openTest(t)
	ctx := context.Background()
	var q rollup.Quality
	q.Requests, q.Completed = 3, 3
	q.TTFT.Observe(250)
	_ = s.AddQuality(ctx, map[rollup.QualityKey]rollup.Quality{{HourMS: hourMS(22, 1), DeploymentID: 17}: q, {HourMS: hourMS(22, 2), DeploymentID: 18}: q})
	cardA := rollup.GPUKey{NodeUUID: "n1", GPUIndex: 0, GPUUUID: "GPU-a"}
	cardB := rollup.GPUKey{NodeUUID: "n2", GPUIndex: 0, GPUUUID: "GPU-b"}
	at := func(k rollup.GPUKey, hour int) rollup.GPUKey { k.HourMS = hourMS(22, hour); return k }
	g := rollup.GPU{UtilSum: 100, UtilN: 2, UtilMax: 60, PowerSum: 600, PowerN: 2, TempSum: 120, TempN: 2, TempMax: 61}
	_ = s.AddGPUs(ctx, map[rollup.GPUKey]rollup.GPU{at(cardA, 1): g, at(cardA, 2): g, at(cardB, 1): g},
		map[rollup.GPUKey]string{cardA: "RTX 6000D", cardB: "PPU 810E"})
	_ = s.AddGPUTime(ctx, map[rollup.GPUTimeKey]rollup.GPUTime{{HourMS: hourMS(22, 1), DeploymentID: 17, GPUModel: "RTX 6000D"}: {GPUSeconds: 3600}},
		map[rollup.CapacityKey]rollup.Capacity{{HourMS: hourMS(22, 1), GPUModel: "RTX 6000D"}: {TotalGPUSeconds: 7200}})
	r := Range{From: hourMS(22, 0), To: hourMS(23, 0)}

	quality, err := s.Quality(ctx, r, 0)
	if err != nil || len(quality) != 2 || quality[0].TTFT.Count() != 1 {
		t.Fatalf("quality = %+v %v", quality, err)
	}
	if one, _ := s.Quality(ctx, r, 18); len(one) != 1 || one[0].DeploymentID != 18 {
		t.Fatalf("deployment 18 = %+v", one)
	}
	cards, err := s.GPUCards(ctx, r, GPUFilter{})
	if err != nil || len(cards) != 2 {
		t.Fatalf("cards = %+v %v", cards, err)
	}
	for _, c := range cards {
		if c.GPUUUID == "GPU-a" && (c.UtilSum != 200 || c.UtilN != 4 || c.GPUModel != "RTX 6000D") {
			t.Fatalf("card a = %+v", c)
		}
	}
	if only, _ := s.GPUCards(ctx, r, GPUFilter{GPUModel: "PPU 810E"}); len(only) != 1 || only[0].NodeUUID != "n2" {
		t.Fatalf("by model = %+v", only)
	}
	if only, _ := s.GPUCards(ctx, r, GPUFilter{NodeUUIDs: []string{"n2"}}); len(only) != 1 || only[0].GPUUUID != "GPU-b" || only[0].UtilSum != 100 {
		t.Fatalf("node n2's cards = %+v", only)
	}
	if none, err := s.GPUCards(ctx, r, GPUFilter{NodeUUIDs: []string{}}); err != nil || len(none) != 0 {
		t.Fatalf("no nodes = %+v %v", none, err)
	}
	for by, want := range map[GPUGroupBy]int{GPUByModel: 3, GPUByNode: 3, GPUByCard: 3} {
		cells, err := s.GPUHeat(ctx, r, GPUFilter{}, by, nil)
		if err != nil || len(cells) != want {
			t.Fatalf("%s heat = %+v %v", by, cells, err)
		}
	}
	if cells, _ := s.GPUHeat(ctx, r, GPUFilter{NodeUUIDs: []string{"n1"}}, GPUByCard, nil); len(cells) != 2 || cells[0].Group != "n1/0/GPU-a" {
		t.Fatalf("card heat = %+v", cells)
	}
	// A node heat map reads only the nodes it shows.
	if cells, _ := s.GPUHeat(ctx, r, GPUFilter{}, GPUByNode, []string{"n2"}); len(cells) != 1 || cells[0].Group != "n2" || cells[0].Bucket != hourMS(22, 1) {
		t.Fatalf("n2 heat = %+v", cells)
	}
	if _, err := s.GPUHeat(ctx, r, GPUFilter{}, "rack", nil); err == nil {
		t.Fatal("an unknown grouping was accepted")
	}
	// Groups sum the range from the model rows, or the node rows when nodes
	// are named.
	groups := func(f GPUFilter, by GPUGroupBy) map[string]float64 {
		t.Helper()
		rows, err := s.GPUGroups(ctx, r, f, by)
		if err != nil {
			t.Fatal(err)
		}
		out := map[string]float64{}
		for _, g := range rows {
			out[g.Group] = g.UtilSum
		}
		return out
	}
	for _, c := range []struct {
		f    GPUFilter
		by   GPUGroupBy
		want string
	}{
		{GPUFilter{}, GPUByModel, "map[PPU 810E:100 RTX 6000D:200]"},
		{GPUFilter{}, GPUByNode, "map[n1:200 n2:100]"},
		{GPUFilter{NodeUUIDs: []string{"n1"}}, GPUByModel, "map[RTX 6000D:200]"},
		{GPUFilter{GPUModel: "PPU 810E"}, GPUByNode, "map[n2:100]"},
		{GPUFilter{NodeUUIDs: []string{}}, GPUByNode, "map[]"},
	} {
		if got := fmt.Sprint(groups(c.f, c.by)); got != c.want {
			t.Fatalf("%+v by %s = %s, want %s", c.f, c.by, got, c.want)
		}
	}
	times, err := s.GPUTime(ctx, r, 0)
	if err != nil || len(times) != 1 || times[0].GPUSeconds != 3600 {
		t.Fatalf("gpu time = %+v %v", times, err)
	}
	if none, _ := s.GPUTime(ctx, r, 99); len(none) != 0 {
		t.Fatal("deployment filter")
	}
	capacity, err := s.Capacity(ctx, r)
	if err != nil || len(capacity) != 1 || capacity[0].TotalGPUSeconds != 7200 {
		t.Fatalf("capacity = %+v %v", capacity, err)
	}
	shanghai := time.UTC
	if err := s.RollupDay(ctx, 20261022, shanghai); err != nil {
		t.Fatal(err)
	}
	days, err := s.RolledDays(ctx, 20261020, 20261030)
	if err != nil || !days[20261022] || len(days) != 1 {
		t.Fatalf("rolled = %v %v", days, err)
	}
	daily := Range{Daily: true, From: 20261022, To: 20261023}
	if dq, _ := s.Quality(ctx, daily, 0); len(dq) != 2 || dq[0].Bucket != 20261022 {
		t.Fatalf("daily quality = %+v", dq)
	}
	if dc, _ := s.GPUCards(ctx, daily, GPUFilter{}); len(dc) != 2 {
		t.Fatalf("daily cards = %+v", dc)
	}
	if dc, _ := s.GPUCards(ctx, daily, GPUFilter{NodeUUIDs: []string{"n1"}}); len(dc) != 1 || dc[0].UtilSum != 200 {
		t.Fatalf("daily n1 cards = %+v", dc)
	}
	if dg, _ := s.GPUGroups(ctx, daily, GPUFilter{}, GPUByNode); len(dg) != 2 {
		t.Fatalf("daily node groups = %+v", dg)
	}
}
