package analytics

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/rollup"
	"gpuworks.ai/gpuworks/internal/server/statsdb"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The analytics scale probe measures what the fleet cases cannot: the
// queries and the background jobs over a full retention's history at the
// scale target. The fleet cases run minutes, so their statistics tables are
// near empty; here the tables are filled straight in SQL with 32 days of
// hours and 401 days of days for every key x deployment pair, deployment
// and card (one day past each default retention, so retention has a day to
// delete), then each page query and job is timed against its budget:
//
//   - every query, its JSON encoding included: p95 under 1 s;
//   - a day's roll-up and a day's retention, per store: under 30 s.
//
// Every pair, deployment and card is active every hour: the most rows the
// target can make. It is a load measurement, gated as the fleet cases are:
//
//	GPUWORKS_TEST_PERF=1 go test -count=1 -v -timeout 60m ./internal/server/analytics/ -run '^TestAnalyticsScale$'
//
// GPUWORKS_ANALYTICS_KEYS, _DEPLOYMENTS, _NODES and _CARDS_PER_NODE change
// the scale (defaults 1000, 500, 1000 and 8).
func TestAnalyticsScale(t *testing.T) {
	if os.Getenv("GPUWORKS_TEST_PERF") != "1" {
		t.Skip("analytics load probe; set GPUWORKS_TEST_PERF=1 on an idle host to run")
	}
	sc := scaleFromEnv(t)
	loc, err := time.LoadLocation("Asia/Shanghai")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Date(2026, 10, 22, 15, 30, 0, 0, loc)
	// GPUWORKS_ANALYTICS_DIR keeps the seeded files for another run (seeding
	// the default scale takes minutes); a directory holding them is reused.
	dir := os.Getenv("GPUWORKS_ANALYTICS_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	_, statErr := os.Stat(filepath.Join(dir, "stats.db"))
	reuse := statErr == nil
	st, err := store.Open(filepath.Join(dir, "gpuworks.db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	opened := time.Now()
	stats, err := statsdb.Open(filepath.Join(dir, "stats.db"))
	if err != nil {
		t.Fatal(err)
	}
	// A kept file of an older schema is upgraded here: the time is the upgrade's.
	t.Logf("stats.db opened in %v", time.Since(opened).Round(time.Millisecond))
	t.Cleanup(func() { _ = stats.Close() })
	retention := store.DefaultDataRetention(72*time.Hour, "Asia/Shanghai")
	svc := &Service{Store: st, Stats: stats, Retention: func() store.DataRetention { return retention },
		LiveSinceMS: now.AddDate(0, 0, -40).UnixMilli(), Now: func() time.Time { return now }}

	var inv inventory
	if reuse {
		inv = readInventory(t, st)
		t.Logf("reusing the history in %s", dir)
	} else {
		start := time.Now()
		inv = seedInventory(t, st, sc)
		t.Logf("inventory: %d deployments, %d keys, %d nodes, %d cards, %d instances in %v",
			len(inv.deployments), len(inv.keys), len(inv.nodes), len(inv.nodes)*sc.cardsPerNode, len(inv.instances), time.Since(start).Round(time.Millisecond))
		start = time.Now()
		g := newHistory(now, loc, retention, sc, inv)
		g.seedUsage(t, st)
		g.seedStats(t, filepath.Join(dir, "stats.db"))
		t.Logf("history seeded in %v", time.Since(start).Round(time.Second))
	}
	ctx := context.Background()
	uh, ud, err := st.UsageRowCounts(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Logf("usage rows: %d hourly, %d daily", uh, ud)
	for _, kind := range statsdb.Kinds() {
		h, d, err := stats.RowCount(ctx, kind)
		if err != nil {
			t.Fatal(err)
		}
		t.Logf("%s rows: %d hourly, %d daily", kind, h, d)
	}
	t.Logf("gpuworks.db %s, stats.db %s", mib(fileSize(filepath.Join(dir, "gpuworks.db"))), mib(stats.SizeBytes()))

	day := func(back int) (int64, int64) {
		d := rollup.DayOf(now.AddDate(0, 0, -back), loc)
		from, _ := d.Bounds(loc)
		return from, now.UnixMilli()
	}
	todayFrom, nowMS := day(0)
	week, _ := day(6)
	month, _ := day(29)
	year, _ := day(399)
	lastMonthFrom := time.Date(2026, 9, 1, 0, 0, 0, 0, loc).UnixMilli()
	lastMonthTo := time.Date(2026, 10, 1, 0, 0, 0, 0, loc).UnixMilli()
	hour := func(from, to int64) Query { return Query{FromMS: from, ToMS: to, Granularity: Hour} }
	byDay := func(from, to int64) Query { return Query{FromMS: from, ToMS: to, Granularity: Day} }
	node := inv.nodes[len(inv.nodes)/2]
	key := inv.keys[len(inv.keys)/2]
	dep := inv.deployments[len(inv.deployments)/2]
	todayDay := rollup.DayOf(now, loc)
	weekDay := rollup.DayOf(now.AddDate(0, 0, -6), loc)

	type probe struct {
		name string
		run  func() (any, error)
	}
	probes := []probe{
		{"usage today by hour", func() (any, error) { return svc.Usage(ctx, hour(todayFrom, nowMS), UsageFilter{}) }},
		{"usage 7d by hour", func() (any, error) { return svc.Usage(ctx, hour(week, nowMS), UsageFilter{}) }},
		{"usage 7d by hour, one key", func() (any, error) { return svc.Usage(ctx, hour(week, nowMS), UsageFilter{APIKeyID: key}) }},
		{"usage 7d by hour, one deployment", func() (any, error) {
			return svc.Usage(ctx, hour(week, nowMS), UsageFilter{DeploymentID: dep})
		}},
		{"usage 30d by day", func() (any, error) { return svc.Usage(ctx, byDay(month, nowMS), UsageFilter{}) }},
		{"usage last month by day", func() (any, error) { return svc.Usage(ctx, byDay(lastMonthFrom, lastMonthTo), UsageFilter{}) }},
		{"usage 400d by day", func() (any, error) { return svc.Usage(ctx, byDay(year, nowMS), UsageFilter{}) }},
		{"API keys today and 7d", func() (any, error) {
			from, _ := todayDay.Bounds(loc)
			return st.UsageByKey(ctx, int32(weekDay), int32(todayDay), from, nowMS)
		}},
		{"quality today by hour", func() (any, error) { return svc.Quality(ctx, hour(todayFrom, nowMS), 0) }},
		{"quality 7d by hour", func() (any, error) { return svc.Quality(ctx, hour(week, nowMS), 0) }},
		{"quality 30d by day", func() (any, error) { return svc.Quality(ctx, byDay(month, nowMS), 0) }},
		{"quality 400d by day", func() (any, error) { return svc.Quality(ctx, byDay(year, nowMS), 0) }},
		{"GPUs today by hour, by model", func() (any, error) {
			return svc.GPUs(ctx, GPUQuery{Query: hour(todayFrom, nowMS), GroupBy: "model"})
		}},
		{"GPUs 7d by hour, by node", func() (any, error) {
			return svc.GPUs(ctx, GPUQuery{Query: hour(week, nowMS), GroupBy: "node"})
		}},
		{"GPUs 7d by hour, one node by card", func() (any, error) {
			return svc.GPUs(ctx, GPUQuery{Query: hour(week, nowMS), GroupBy: "gpu", NodeUUIDs: []string{node}})
		}},
		{"GPUs 7d by hour, 30 nodes by card", func() (any, error) {
			return svc.GPUs(ctx, GPUQuery{Query: hour(week, nowMS), GroupBy: "gpu", NodeUUIDs: inv.nodes[:30]})
		}},
		{"GPUs 30d by day, half the nodes by node", func() (any, error) {
			return svc.GPUs(ctx, GPUQuery{Query: byDay(month, nowMS), GroupBy: "node", NodeUUIDs: inv.nodes[:len(inv.nodes)/2]})
		}},
		{"GPUs 7d by hour, one model by node", func() (any, error) {
			return svc.GPUs(ctx, GPUQuery{Query: hour(week, nowMS), GroupBy: "node", GPUModel: inv.models[0]})
		}},
		{"GPUs 400d by day, by node", func() (any, error) {
			return svc.GPUs(ctx, GPUQuery{Query: byDay(year, nowMS), GroupBy: "node"})
		}},
		{"GPUs 30d by day, by node", func() (any, error) {
			return svc.GPUs(ctx, GPUQuery{Query: byDay(month, nowMS), GroupBy: "node"})
		}},
		{"GPUs 400d by day, by model", func() (any, error) {
			return svc.GPUs(ctx, GPUQuery{Query: byDay(year, nowMS), GroupBy: "model"})
		}},
		{"GPU time 7d by hour", func() (any, error) { return svc.GPUTime(ctx, hour(week, nowMS), 0) }},
		{"GPU time 30d by day", func() (any, error) { return svc.GPUTime(ctx, byDay(month, nowMS), 0) }},
		{"GPU time 400d by day", func() (any, error) { return svc.GPUTime(ctx, byDay(year, nowMS), 0) }},
	}
	// GPUWORKS_ANALYTICS_PROBE runs only the probes whose name contains it.
	if only := os.Getenv("GPUWORKS_ANALYTICS_PROBE"); only != "" {
		probes = slices.DeleteFunc(probes, func(p probe) bool { return !strings.Contains(p.name, only) })
	}
	const runs = 10
	queryBudget := time.Second
	for _, p := range probes {
		times := make([]time.Duration, 0, runs)
		var size int
		for range runs {
			began := time.Now()
			out, err := p.run()
			if err != nil {
				t.Fatalf("%s: %v", p.name, err)
			}
			body, err := json.Marshal(out)
			if err != nil {
				t.Fatalf("%s: encode: %v", p.name, err)
			}
			times = append(times, time.Since(began))
			size = len(body)
		}
		first := times[0]
		slices.Sort(times)
		p95 := times[(len(times)*95+99)/100-1]
		t.Logf("%-36s first %8v  p50 %8v  p95 %8v  %s", p.name, first.Round(time.Millisecond),
			times[len(times)/2].Round(time.Millisecond), p95.Round(time.Millisecond), kib(size))
		if p95 > queryBudget {
			t.Errorf("%s: p95 %v over the %v budget", p.name, p95, queryBudget)
		}
	}

	if os.Getenv("GPUWORKS_ANALYTICS_DIR") != "" {
		t.Log("jobs skipped: retention would change the kept history")
		return
	}
	jobBudget := 30 * time.Second
	job := func(name string, fn func() error) {
		began := time.Now()
		if err := fn(); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		took := time.Since(began)
		t.Logf("%-36s %v", name, took.Round(time.Millisecond))
		if took > jobBudget {
			t.Errorf("%s: %v over the %v budget", name, took, jobBudget)
		}
	}
	yesterday := rollup.DayOf(now.AddDate(0, 0, -1), loc)
	job("usage roll-up of a day", func() error { return st.RollupUsageDay(ctx, yesterday, loc) })
	job("stats roll-up of a day", func() error { return stats.RollupDay(ctx, yesterday, loc) })
	hourCutoff := rollup.HourMS(now.AddDate(0, 0, -retention.UsageHourlyDays))
	dayCutoff := rollup.DayOf(now.AddDate(0, 0, -retention.UsageDailyDays), loc)
	var deleted int64
	job("usage retention of a day", func() (err error) {
		deleted, err = st.RetainUsage(ctx, hourCutoff, dayCutoff)
		return err
	})
	t.Logf("usage retention deleted %d rows", deleted)
	if deleted == 0 {
		t.Error("usage retention deleted nothing: the history holds no day past the retention")
	}
	job("stats retention of a day", func() (err error) {
		deleted, err = stats.Retain(ctx, hourCutoff, dayCutoff)
		return err
	})
	t.Logf("stats retention deleted %d rows", deleted)
	if deleted == 0 {
		t.Error("stats retention deleted nothing: the history holds no day past the retention")
	}
}

type scale struct {
	keys, deployments, nodes, cardsPerNode int
}

func scaleFromEnv(t *testing.T) scale {
	sc := scale{keys: 1000, deployments: 500, nodes: 1000, cardsPerNode: 8}
	for name, dest := range map[string]*int{
		"GPUWORKS_ANALYTICS_KEYS": &sc.keys, "GPUWORKS_ANALYTICS_DEPLOYMENTS": &sc.deployments,
		"GPUWORKS_ANALYTICS_NODES": &sc.nodes, "GPUWORKS_ANALYTICS_CARDS_PER_NODE": &sc.cardsPerNode,
	} {
		if v := os.Getenv(name); v != "" {
			n, err := strconv.Atoi(v)
			if err != nil || n <= 0 {
				t.Fatalf("%s=%q: a positive count", name, v)
			}
			*dest = n
		}
	}
	return sc
}

type inventory struct {
	deployments []uint
	keys        []uint
	nodes       []string // UUIDs
	instances   []uint
	models      []string
}

// seedInventory makes the deployments, keys, online nodes with their cards
// and an instance per deployment on every card of its share of the nodes,
// so the pages' names and the cards held now are read at scale too.
func seedInventory(t *testing.T, st *store.Store, sc scale) inventory {
	t.Helper()
	inv := inventory{models: []string{"NVIDIA H100 80GB HBM3", "NVIDIA A100-SXM4-80GB", "NVIDIA L40S", "NVIDIA GeForce RTX 4090"}}
	db := st.DB()
	for i := range sc.deployments {
		d, err := st.CreateDeployment(&store.Deployment{Name: fmt.Sprintf("dep-%03d", i), ServedName: fmt.Sprintf("org/model-%03d", i), State: store.DeployActive})
		if err != nil {
			t.Fatal(err)
		}
		inv.deployments = append(inv.deployments, d.ID)
	}
	keys := make([]store.APIKey, sc.keys)
	for i := range keys {
		keys[i] = store.APIKey{Name: fmt.Sprintf("customer-%04d", i), Prefix: fmt.Sprintf("gw-%08x", i), Hash: "x", CreatedAt: time.Now().UTC()}
	}
	if err := db.CreateInBatches(keys, 500).Error; err != nil {
		t.Fatal(err)
	}
	for _, k := range keys {
		inv.keys = append(inv.keys, k.ID)
	}
	nodes := make([]store.Node, sc.nodes)
	for i := range nodes {
		nodes[i] = store.Node{UUID: fmt.Sprintf("node-%04d", i), Name: fmt.Sprintf("gpu-%04d", i), State: store.NodeStateOnline}
	}
	if err := db.CreateInBatches(nodes, 500).Error; err != nil {
		t.Fatal(err)
	}
	var cards []store.GPU
	for i, n := range nodes {
		inv.nodes = append(inv.nodes, n.UUID)
		for c := range sc.cardsPerNode {
			cards = append(cards, store.GPU{NodeID: n.ID, Ident: fmt.Sprintf("GPU-%04d-%d", i, c), Index: c,
				UUID: fmt.Sprintf("GPU-%04d-%d", i, c), Name: inv.models[i%len(inv.models)], MemoryTotal: 80 << 30})
		}
	}
	if err := db.CreateInBatches(cards, 1000).Error; err != nil {
		t.Fatal(err)
	}
	// Every node runs one deployment on all its cards: 1000 nodes, 500
	// deployments, two instances each.
	var insts []store.Instance
	for i, n := range nodes {
		idents := make([]string, sc.cardsPerNode)
		for c := range idents {
			idents[c] = fmt.Sprintf("GPU-%04d-%d", i, c)
		}
		raw, _ := json.Marshal(idents)
		insts = append(insts, store.Instance{UUID: fmt.Sprintf("inst-%04d", i), DeploymentID: inv.deployments[i%len(inv.deployments)],
			NodeID: n.ID, State: proto.InstRunning, GPUIdents: string(raw)})
	}
	if err := db.CreateInBatches(insts, 500).Error; err != nil {
		t.Fatal(err)
	}
	for _, in := range insts {
		inv.instances = append(inv.instances, in.ID)
	}
	return inv
}

// history writes the seeded rows: hours from hourFrom up to now's hour,
// days from dayFrom up to today (today has hours only, as it does live).
type history struct {
	loc             *time.Location
	sc              scale
	inv             inventory
	hours           []int64
	days            []rollup.Day
	ttft, e2e       []byte
	qualityPerHour  rollup.Quality
	qualityPerDay   rollup.Quality
	ttftDay, e2eDay []byte
}

func newHistory(now time.Time, loc *time.Location, r store.DataRetention, sc scale, inv inventory) *history {
	g := &history{loc: loc, sc: sc, inv: inv}
	end := rollup.HourMS(now)
	for h := rollup.HourMS(now.AddDate(0, 0, -(r.UsageHourlyDays + 1))); h <= end; h += hourMS {
		g.hours = append(g.hours, h)
	}
	today := rollup.DayOf(now, loc)
	for d := rollup.DayOf(now.AddDate(0, 0, -(r.UsageDailyDays+1)), loc); d < today; d = d.Next() {
		g.days = append(g.days, d)
	}
	q := rollup.Quality{Requests: 600, Completed: 590, Length: 4, ClientClosed: 3, Interrupted: 2, Error: 1, OutputTokens: 180000, DecodeMS: 3600000}
	for i := range 600 {
		q.TTFT.Observe(float64(80 + i*3))
		q.EndToEnd.Observe(float64(900 + i*40))
	}
	g.qualityPerHour = q
	g.ttft, g.e2e = q.TTFT.Encode(), q.EndToEnd.Encode()
	var day rollup.Quality
	for range 24 {
		day.Add(q)
	}
	g.qualityPerDay = day
	g.ttftDay, g.e2eDay = day.TTFT.Encode(), day.EndToEnd.Encode()
	return g
}

// pairs are the key x deployment pairs: every key uses two deployments.
func (g *history) pairs() [][2]uint {
	out := make([][2]uint, 0, 2*len(g.inv.keys))
	n := len(g.inv.deployments)
	for i, k := range g.inv.keys {
		out = append(out, [2]uint{k, g.inv.deployments[i%n]}, [2]uint{k, g.inv.deployments[(i*7+3)%n]})
	}
	return out
}

func (g *history) seedUsage(t *testing.T, st *store.Store) {
	t.Helper()
	db, err := st.DB().DB()
	if err != nil {
		t.Fatal(err)
	}
	pairs := g.pairs()
	cols := `requests, no_usage, prompt_tokens, completion_tokens, reasoning_tokens, reasoning_reported, cached_tokens, cache_reported, cache_reported_prompt`
	counts := func(scale int64) []any {
		return []any{60 * scale, 0, 900000 * scale, 30000 * scale, 4000 * scale, 10 * scale, 600000 * scale, 60 * scale, 900000 * scale}
	}
	hourly, daily := counts(1), counts(24)
	bulk(t, db, `INSERT INTO usage_hourly (hour_ms, api_key_id, user_id, deployment_id, source, `+cols+`) VALUES (?, ?, 0, ?, 0, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		len(g.hours)*len(pairs), func(i int) []any {
			p := pairs[i%len(pairs)]
			return append([]any{g.hours[i/len(pairs)], p[0], p[1]}, hourly...)
		})
	bulk(t, db, `INSERT INTO usage_daily (day, api_key_id, user_id, deployment_id, source, `+cols+`) VALUES (?, ?, 0, ?, 0, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		len(g.days)*len(pairs), func(i int) []any {
			p := pairs[i%len(pairs)]
			return append([]any{int32(g.days[i/len(pairs)]), p[0], p[1]}, daily...)
		})
	bulk(t, db, `INSERT INTO usage_rolled_days (day) VALUES (?)`, len(g.days), func(i int) []any { return []any{int32(g.days[i])} })
}

func (g *history) seedStats(t *testing.T, path string) {
	t.Helper()
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	db.SetMaxOpenConns(1)
	deps := g.inv.deployments
	q := func(hist, e2e []byte, v rollup.Quality) []any {
		return []any{v.Requests, v.Completed, v.Length, v.ClientClosed, v.Interrupted, v.Error, hist, e2e, v.OutputTokens, v.DecodeMS}
	}
	bulk(t, db, `INSERT INTO quality_hourly VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, len(g.hours)*len(deps), func(i int) []any {
		return append([]any{g.hours[i/len(deps)], deps[i%len(deps)]}, q(g.ttft, g.e2e, g.qualityPerHour)...)
	})
	bulk(t, db, `INSERT INTO quality_daily VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, len(g.days)*len(deps), func(i int) []any {
		return append([]any{int64(g.days[i/len(deps)]), deps[i%len(deps)]}, q(g.ttftDay, g.e2eDay, g.qualityPerDay)...)
	})
	cards := len(g.inv.nodes) * g.sc.cardsPerNode
	card := func(i int) []any {
		c := i % cards
		n := c / g.sc.cardsPerNode
		idx := c % g.sc.cardsPerNode
		return []any{g.inv.nodes[n], idx, fmt.Sprintf("GPU-%04d-%d", n, idx), g.inv.models[n%len(g.inv.models)]}
	}
	gpu := func(samples float64) []any {
		return []any{37 * samples, int64(samples), 100.0, 60e9 * samples, int64(samples), 70e9, 80e9, 350 * samples, int64(samples), 60 * samples, int64(samples), 75.0}
	}
	perHour, perDay := gpu(240), gpu(240*24)
	bulk(t, db, `INSERT INTO gpu_hourly VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, len(g.hours)*cards, func(i int) []any {
		return append(append([]any{g.hours[i/cards]}, card(i)...), perHour...)
	})
	bulk(t, db, `INSERT INTO gpu_daily VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`, len(g.days)*cards, func(i int) []any {
		return append(append([]any{int64(g.days[i/cards])}, card(i)...), perDay...)
	})
	// Each deployment holds the cards of its share of the nodes, one model.
	held := float64(len(g.inv.nodes) * g.sc.cardsPerNode / len(deps))
	model := func(i int) string { return g.inv.models[(i%len(deps))%len(g.inv.models)] }
	bulk(t, db, `INSERT INTO gpu_time_hourly VALUES (?, ?, ?, ?, ?, ?, ?)`, len(g.hours)*len(deps), func(i int) []any {
		return []any{g.hours[i/len(deps)], deps[i%len(deps)], model(i), held * 3600, held * 3600, 2 * 3600.0, 2}
	})
	bulk(t, db, `INSERT INTO gpu_time_daily VALUES (?, ?, ?, ?, ?, ?, ?)`, len(g.days)*len(deps), func(i int) []any {
		return []any{int64(g.days[i/len(deps)]), deps[i%len(deps)], model(i), held * 86400, held * 86400, 2 * 86400.0, 2}
	})
	perModel := float64(cards / len(g.inv.models))
	models := len(g.inv.models)
	bulk(t, db, `INSERT INTO gpu_capacity_hourly VALUES (?, ?, ?, ?)`, len(g.hours)*models, func(i int) []any {
		return []any{g.hours[i/models], g.inv.models[i%models], perModel * 3600, perModel * 3600}
	})
	bulk(t, db, `INSERT INTO gpu_capacity_daily VALUES (?, ?, ?, ?)`, len(g.days)*models, func(i int) []any {
		return []any{int64(g.days[i/models]), g.inv.models[i%models], perModel * 86400, perModel * 86400}
	})
	bulk(t, db, `INSERT INTO rolled_days (day) VALUES (?)`, len(g.days), func(i int) []any { return []any{int64(g.days[i])} })
	// The node and model rows are the cards' summed, as AddGPUs and the day
	// roll-up write them.
	for _, stmt := range []string{
		`INSERT INTO gpu_node_hourly SELECT hour_ms, node_uuid, gpu_model, ` + statsSums + ` FROM gpu_hourly GROUP BY hour_ms, node_uuid, gpu_model`,
		`INSERT INTO gpu_node_daily SELECT day, node_uuid, gpu_model, ` + statsSums + ` FROM gpu_daily GROUP BY day, node_uuid, gpu_model`,
		`INSERT INTO gpu_model_hourly SELECT hour_ms, gpu_model, ` + statsSums + ` FROM gpu_hourly GROUP BY hour_ms, gpu_model`,
		`INSERT INTO gpu_model_daily SELECT day, gpu_model, ` + statsSums + ` FROM gpu_daily GROUP BY day, gpu_model`,
	} {
		if _, err := db.Exec(stmt); err != nil {
			t.Fatal(err)
		}
	}
}

// readInventory reads back what seedInventory made.
func readInventory(t *testing.T, st *store.Store) inventory {
	t.Helper()
	inv := inventory{models: []string{"NVIDIA H100 80GB HBM3", "NVIDIA A100-SXM4-80GB", "NVIDIA L40S", "NVIDIA GeForce RTX 4090"}}
	db := st.DB()
	for _, q := range []struct {
		table, column string
		dest          any
	}{
		{"deployments", "id", &inv.deployments}, {"api_keys", "id", &inv.keys}, {"nodes", "uuid", &inv.nodes}, {"instances", "id", &inv.instances},
	} {
		if err := db.Table(q.table).Order("id").Pluck(q.column, q.dest).Error; err != nil {
			t.Fatal(err)
		}
	}
	return inv
}

const statsSums = `SUM(util_sum), SUM(util_n), MAX(util_max), SUM(mem_used_sum), SUM(mem_used_n), MAX(mem_used_max), MAX(mem_total),
	SUM(power_sum), SUM(power_n), SUM(temp_sum), SUM(temp_n), MAX(temp_max)`

// bulk inserts n rows, 50,000 to a transaction.
func bulk(t *testing.T, db *sql.DB, stmt string, n int, row func(int) []any) {
	t.Helper()
	const perTx = 50_000
	for at := 0; at < n; at += perTx {
		tx, err := db.Begin()
		if err != nil {
			t.Fatal(err)
		}
		prep, err := tx.Prepare(stmt)
		if err != nil {
			t.Fatal(err)
		}
		for i := at; i < min(at+perTx, n); i++ {
			if _, err := prep.Exec(row(i)...); err != nil {
				t.Fatalf("%s: %v", stmt, err)
			}
		}
		prep.Close()
		if err := tx.Commit(); err != nil {
			t.Fatal(err)
		}
	}
}

func fileSize(path string) int64 {
	var total int64
	for _, suffix := range []string{"", "-wal"} {
		if info, err := os.Stat(path + suffix); err == nil {
			total += info.Size()
		}
	}
	return total
}

func mib(n int64) string { return fmt.Sprintf("%.0f MiB", float64(n)/(1<<20)) }
func kib(n int) string   { return fmt.Sprintf("%.0f KiB", float64(n)/(1<<10)) }
