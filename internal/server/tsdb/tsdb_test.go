package tsdb

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"log/slog"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"
)

const metricUtil = "gpuworks_gpu_utilization"

func openTemp(t *testing.T) Store {
	t.Helper()
	path := filepath.Join(t.TempDir(), "metrics.db")
	s, err := OpenSQLite(path)
	if err != nil {
		t.Fatalf("open: %v", err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}

// sample builds a Sample for the given gpu at time t with value v.
func sample(gpu string, t, v int64) Sample {
	return Sample{
		Metric: metricUtil,
		Labels: map[string]string{"node": "node-1", "gpu": gpu},
		Value:  float64(v),
		TimeMS: t,
	}
}

func TestWriteAndQueryRangeSelectsSeries(t *testing.T) {
	s := openTemp(t)
	// Two GPUs, several timestamps each. Label map order intentionally varies.
	batch := []Sample{
		sample("0", 1000, 10),
		sample("1", 1000, 90),
		sample("0", 2000, 20),
		sample("1", 2000, 80),
		sample("0", 3000, 30),
		sample("1", 3000, 70),
	}
	if err := s.WriteBatch(batch); err != nil {
		t.Fatalf("write: %v", err)
	}

	// Matcher selecting only GPU 0.
	got, err := s.QueryRange(metricUtil, map[string]string{"gpu": "0"}, 0, 10000, 1000)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d series, want 1", len(got))
	}
	if got[0].Labels["gpu"] != "0" || got[0].Labels["node"] != "node-1" {
		t.Fatalf("wrong labels: %v", got[0].Labels)
	}
	wantVals := []float64{10, 20, 30}
	if len(got[0].Points) != len(wantVals) {
		t.Fatalf("got %d points, want %d: %+v", len(got[0].Points), len(wantVals), got[0].Points)
	}
	for i, p := range got[0].Points {
		if p.Value != wantVals[i] {
			t.Errorf("point %d value = %v, want %v", i, p.Value, wantVals[i])
		}
	}

	// No matcher -> both series returned.
	all, err := s.QueryRange(metricUtil, nil, 0, 10000, 1000)
	if err != nil {
		t.Fatalf("query all: %v", err)
	}
	if len(all) != 2 {
		t.Fatalf("got %d series, want 2", len(all))
	}
}

// TestQueryRangeEmptyResultMarshalsAsEmptyArray pins the wire contract: a
// query matching no series must serialize as JSON `[]`, not `null` — the
// difference between "empty" and "missing" to a caller (monitor/monitorapi)
// that round-trips this through encoding/json.
func TestQueryRangeEmptyResultMarshalsAsEmptyArray(t *testing.T) {
	s := openTemp(t)
	got, err := s.QueryRange("no_such_metric", nil, 0, 10000, 1000)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if got == nil {
		t.Fatal("QueryRange returned a nil slice for an empty result, want a non-nil empty slice")
	}
	b, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "[]" {
		t.Fatalf("json = %s, want []", b)
	}
}

// TestQueryRangeSeriesWithSamplesOnlyOutsideWindowHasEmptyPointsArray covers
// the per-series half of the same contract: a series that matches the
// metric/label filter but has no samples inside [startMS, endMS] (the LEFT
// JOIN's "no row" case) must still appear with Points marshaling as `[]`,
// not `null`.
func TestQueryRangeSeriesWithSamplesOnlyOutsideWindowHasEmptyPointsArray(t *testing.T) {
	s := openTemp(t)
	if err := s.WriteBatch([]Sample{sample("0", 50_000, 1)}); err != nil {
		t.Fatalf("write: %v", err)
	}
	// The window [0, 10000) excludes the only sample (at ts=50000).
	got, err := s.QueryRange(metricUtil, map[string]string{"gpu": "0"}, 0, 10000, 1000)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d series, want 1 (the series itself still matches)", len(got))
	}
	if got[0].Points == nil {
		t.Fatal("Points is nil, want a non-nil empty slice")
	}
	if len(got[0].Points) != 0 {
		t.Fatalf("got %d points, want 0: %+v", len(got[0].Points), got[0].Points)
	}
	b, err := json.Marshal(got[0].Points)
	if err != nil {
		t.Fatal(err)
	}
	if string(b) != "[]" {
		t.Fatalf("Points json = %s, want []", b)
	}
}

func TestQueryRangeBucketingKeepsLastValue(t *testing.T) {
	s := openTemp(t)
	// Three samples inside one 10s bucket [0,10000): last one must win.
	// Plus one sample in the next bucket.
	batch := []Sample{
		sample("0", 1000, 1),
		sample("0", 5000, 2),
		sample("0", 9000, 3), // last in bucket 0
		sample("0", 12000, 7),
	}
	if err := s.WriteBatch(batch); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := s.QueryRange(metricUtil, map[string]string{"gpu": "0"}, 0, 20000, 10000)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d series, want 1", len(got))
	}
	pts := got[0].Points
	if len(pts) != 2 {
		t.Fatalf("got %d buckets, want 2: %+v", len(pts), pts)
	}
	if pts[0].TimeMS != 0 || pts[0].Value != 3 {
		t.Errorf("bucket 0 = %+v, want {0, 3}", pts[0])
	}
	if pts[1].TimeMS != 10000 || pts[1].Value != 7 {
		t.Errorf("bucket 1 = %+v, want {10000, 7}", pts[1])
	}
}

func TestRetainDeletesOldSamples(t *testing.T) {
	s := openTemp(t)
	batch := []Sample{
		sample("0", 1000, 1),
		sample("0", 2000, 2),
		sample("0", 3000, 3),
	}
	if err := s.WriteBatch(batch); err != nil {
		t.Fatalf("write: %v", err)
	}
	// Drop everything strictly before ts=3000.
	if err := s.Retain(context.Background(), 3000); err != nil {
		t.Fatalf("retain: %v", err)
	}
	got, err := s.QueryRange(metricUtil, map[string]string{"gpu": "0"}, 0, 10000, 1000)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d series, want 1", len(got))
	}
	if len(got[0].Points) != 1 {
		t.Fatalf("got %d points after retain, want 1: %+v", len(got[0].Points), got[0].Points)
	}
	if got[0].Points[0].TimeMS != 3000 || got[0].Points[0].Value != 3 {
		t.Errorf("remaining point = %+v, want {3000, 3}", got[0].Points[0])
	}
}

// TestRetainDeletesInBatches pins the exact batch sequence Retain issues: a
// full batch (n == retainBatchSize) keeps looping, a short final batch (n <
// retainBatchSize) stops the loop, and the orphan-series sweep still runs
// once at the end.
func TestRetainDeletesInBatches(t *testing.T) {
	store := openTemp(t)
	s := store.(*sqliteStore)
	oldSize, oldYield := retainBatchSize, retainBatchYield
	retainBatchSize, retainBatchYield = 3, time.Millisecond
	t.Cleanup(func() { retainBatchSize, retainBatchYield = oldSize, oldYield })

	var batch []Sample
	for i := int64(0); i < 7; i++ {
		batch = append(batch, sample("0", 1000+i*100, i))
	}
	if err := s.WriteBatch(batch); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var calls []int64
	s.retainBatchHook = func(n int64) { calls = append(calls, n) }
	if err := s.Retain(context.Background(), 1000+7*100); err != nil {
		t.Fatalf("retain: %v", err)
	}

	want := []int64{3, 3, 1}
	if len(calls) != len(want) {
		t.Fatalf("got %d batch calls %v, want %d %v", len(calls), calls, len(want), want)
	}
	for i, w := range want {
		if calls[i] != w {
			t.Errorf("batch %d = %d, want %d", i, calls[i], w)
		}
	}

	got, err := s.QueryRange(metricUtil, map[string]string{"gpu": "0"}, 0, 10000, 100)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 0 {
		t.Fatalf("expected the fully-drained series to be swept as an orphan, got %+v", got)
	}
}

// TestRetainStopsAtBatchCeilingAndLogsAWarning covers the iteration bound: a
// backlog larger than retainMaxBatches*retainBatchSize must not turn one
// Retain call into an unbounded loop — it stops at the ceiling, logs a
// warning so an operator can see it happened, and leaves the rest for the
// next scheduled call (retainLoop runs every 6h) rather than erroring.
func TestRetainStopsAtBatchCeilingAndLogsAWarning(t *testing.T) {
	store := openTemp(t)
	s := store.(*sqliteStore)
	oldSize, oldYield, oldMax := retainBatchSize, retainBatchYield, retainMaxBatches
	retainBatchSize, retainBatchYield, retainMaxBatches = 2, time.Millisecond, 2
	t.Cleanup(func() { retainBatchSize, retainBatchYield, retainMaxBatches = oldSize, oldYield, oldMax })

	var batch []Sample
	for i := int64(0); i < 10; i++ {
		batch = append(batch, sample("0", 1000+i*100, i))
	}
	if err := s.WriteBatch(batch); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var logs bytes.Buffer
	oldLogger := slog.Default()
	slog.SetDefault(slog.New(slog.NewTextHandler(&logs, nil)))
	t.Cleanup(func() { slog.SetDefault(oldLogger) })

	var batches int
	s.retainBatchHook = func(int64) { batches++ }
	if err := s.Retain(context.Background(), 1000+10*100); err != nil {
		t.Fatalf("retain: %v", err)
	}

	if batches != retainMaxBatches {
		t.Fatalf("batches run = %d, want exactly the ceiling %d", batches, retainMaxBatches)
	}
	if !strings.Contains(logs.String(), "retain hit its per-call batch ceiling") {
		t.Fatalf("expected a warning log when the ceiling is hit, got: %s", logs.String())
	}

	// retainMaxBatches(2) x retainBatchSize(2) = 4 of the 10 rows deleted;
	// 6 remain for the next scheduled Retain call.
	got, err := s.QueryRange(metricUtil, map[string]string{"gpu": "0"}, 0, 10000, 100)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 1 || len(got[0].Points) != 6 {
		t.Fatalf("got %+v, want 1 series with 6 remaining points", got)
	}
}

// TestRetainYieldsConnectionBetweenBatches proves Retain's batching actually
// frees the pool's one connection between passes: a concurrent WriteBatch
// started shortly after Retain begins must complete well before Retain's own
// multi-batch, multi-sleep prune does, instead of queuing behind one long
// transaction the way the pre-batching Retain did.
func TestRetainYieldsConnectionBetweenBatches(t *testing.T) {
	store := openTemp(t)
	s := store.(*sqliteStore)
	oldSize, oldYield := retainBatchSize, retainBatchYield
	retainBatchSize, retainBatchYield = 2, 60*time.Millisecond
	t.Cleanup(func() { retainBatchSize, retainBatchYield = oldSize, oldYield })

	var old []Sample
	for i := int64(0); i < 10; i++ {
		old = append(old, sample("0", 1000+i*100, i))
	}
	if err := s.WriteBatch(old); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var batches int64
	s.retainBatchHook = func(int64) { atomic.AddInt64(&batches, 1) }

	retainDone := make(chan error, 1)
	go func() { retainDone <- s.Retain(context.Background(), 1000+10*100) }()

	// Let Retain issue its first batch and enter the inter-batch sleep before
	// racing a write against it.
	time.Sleep(20 * time.Millisecond)

	writeDone := make(chan error, 1)
	go func() { writeDone <- s.WriteBatch([]Sample{sample("1", 999999, 1)}) }()

	select {
	case err := <-writeDone:
		if err != nil {
			t.Fatalf("concurrent write failed: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("concurrent WriteBatch did not complete promptly; Retain may be holding the connection for the whole prune")
	case <-retainDone:
		t.Fatal("retain finished before the concurrent write completed, so this run cannot show they overlapped")
	}

	select {
	case err := <-retainDone:
		if err != nil {
			t.Fatalf("retain: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("retain did not finish")
	}
	if atomic.LoadInt64(&batches) < 2 {
		t.Fatalf("expected multiple retain batches, got %d", batches)
	}
}

func TestWriteBatchEmpty(t *testing.T) {
	s := openTemp(t)
	if err := s.WriteBatch(nil); err != nil {
		t.Fatalf("empty write: %v", err)
	}
}

func TestQueryLatestReturnsNewestFreshPointPerSeries(t *testing.T) {
	s := openTemp(t)
	otherMetric := Sample{
		Metric: "gpuworks_gpu_temperature_celsius",
		Labels: map[string]string{"node": "node-1", "gpu": "0"},
		Value:  99,
		TimeMS: 4000,
	}
	if err := s.WriteBatch([]Sample{
		sample("0", 1000, 1),
		sample("0", 3000, 3),
		sample("1", 1000, 2), // stale at the requested cutoff
		otherMetric,          // newer, but belongs to another metric
	}); err != nil {
		t.Fatal(err)
	}
	got, err := s.QueryLatest(metricUtil, map[string]string{"gpu": "0"}, 1500)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Value != 3 || got[0].TimeMS != 3000 {
		t.Fatalf("latest = %+v", got)
	}
}

// explainPlan runs EXPLAIN QUERY PLAN for query and returns its detail lines
// joined by newline, for asserting a query seeks an index instead of scanning
// or materializing it.
func explainPlan(t *testing.T, db *sql.DB, query string, args ...any) string {
	t.Helper()
	rows, err := db.Query("EXPLAIN QUERY PLAN "+query, args...)
	if err != nil {
		t.Fatal(err)
	}
	defer rows.Close()
	var details []string
	for rows.Next() {
		var id, parent, unused int
		var detail string
		if err := rows.Scan(&id, &parent, &unused, &detail); err != nil {
			t.Fatal(err)
		}
		details = append(details, detail)
	}
	if err := rows.Err(); err != nil {
		t.Fatal(err)
	}
	return strings.Join(details, "\n")
}

// TestQueryLatestPlanSeeksPerSeriesTimeIndex checks the "recent" per-series
// MAX(ts_ms) subquery seeks its covering index rather than scanning it, both
// unfiltered and with an instance/deployment matcher pushed into the SQL
// WHERE - the filtered forms must not regress the sub-query's plan.
func TestQueryLatestPlanSeeksPerSeriesTimeIndex(t *testing.T) {
	s := openTemp(t).(*sqliteStore)
	cases := []struct {
		name     string
		matchers map[string]string
	}{
		{"unfiltered", nil},
		{"instance filter", map[string]string{"instance": "inst-a"}},
		{"deployment filter", map[string]string{"deployment": "7"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clause, extraArgs, _ := indexedMatcherClause("sr.", tc.matchers)
			query := queryLatestBaseSQL + clause + queryLatestOrderSQL
			args := append([]any{int64(1500), int64(3000), metricUtil}, extraArgs...)
			plan := explainPlan(t, s.db, query, args...)
			if !strings.Contains(plan, "SEARCH recent USING COVERING INDEX") ||
				strings.Contains(plan, "SCAN recent") || strings.Contains(plan, "MATERIALIZE") {
				t.Fatalf("QueryLatest must seek each series by (series_id, ts_ms), plan:\n%s", plan)
			}
		})
	}
}

// TestQueryRangeSingleStatementPlanSeeksSamplesBySeriesID checks the single
// LEFT JOIN statement QueryRange now issues (replacing one SELECT per
// candidate series) still seeks samples by its (series_id, ts_ms) primary key
// instead of scanning the whole table.
func TestQueryRangeSingleStatementPlanSeeksSamplesBySeriesID(t *testing.T) {
	s := openTemp(t).(*sqliteStore)
	cases := []struct {
		name     string
		matchers map[string]string
	}{
		{"unfiltered", nil},
		{"instance filter", map[string]string{"instance": "inst-a"}},
		{"deployment filter", map[string]string{"deployment": "7"}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			clause, extraArgs, _ := indexedMatcherClause("sr.", tc.matchers)
			query := fmt.Sprintf(queryRangeJoinSQL, clause)
			args := append([]any{int64(0), int64(10000), metricUtil}, extraArgs...)
			plan := explainPlan(t, s.db, query, args...)
			if !strings.Contains(plan, "SEARCH sm") || strings.Contains(plan, "SCAN sm") {
				t.Fatalf("QueryRange must seek samples by series_id via the join, plan:\n%s", plan)
			}
			if strings.Contains(plan, "USE TEMP B-TREE FOR ORDER BY") {
				t.Fatalf("QueryRange's ORDER BY sr.id, sm.ts_ms should already be satisfied by the join's own index scans, plan:\n%s", plan)
			}
		})
	}
}

// TestRetainOrphanSweepPlanSeeksSamplesBySeriesID checks the orphan-series
// DELETE seeks samples by series_id instead of scanning the whole table: the
// alternative "id NOT IN (SELECT DISTINCT series_id FROM samples)" form
// forces a full scan/materialization of that subquery and measured ~10x
// slower on a large samples table.
func TestRetainOrphanSweepPlanSeeksSamplesBySeriesID(t *testing.T) {
	s := openTemp(t).(*sqliteStore)
	plan := explainPlan(t, s.db, retainDropOrphanSeriesSQL)
	if !strings.Contains(plan, "SEARCH samples") ||
		strings.Contains(plan, "SCAN samples") || strings.Contains(plan, "MATERIALIZE") {
		t.Fatalf("orphan sweep must seek samples by series_id, plan:\n%s", plan)
	}
}

// inferSample builds a Sample carrying instance/deployment labels, as
// collector.parsePrometheus and collector.dgpuSamples now do.
func inferSample(instance, deployment string, t, v int64) Sample {
	labels := map[string]string{"node": "node-1", "engine": "vllm", "instance": instance}
	if deployment != "" {
		labels["deployment"] = deployment
	}
	return Sample{Metric: "gpuworks_infer_requests_running", Labels: labels, Value: float64(v), TimeMS: t}
}

// TestWriteBatchPopulatesInstanceDeploymentColumns checks the series table's
// indexed instance/deployment columns directly, including the NULL case for a
// metric that carries neither label.
func TestWriteBatchPopulatesInstanceDeploymentColumns(t *testing.T) {
	store := openTemp(t)
	s := store.(*sqliteStore)
	if err := s.WriteBatch([]Sample{
		inferSample("inst-a", "3", 1000, 1),
		sample("0", 1000, 10), // no instance/deployment label at all
	}); err != nil {
		t.Fatalf("write: %v", err)
	}

	var instance, deployment sql.NullString
	if err := s.db.QueryRow(`SELECT instance, deployment FROM series WHERE metric = ?`,
		"gpuworks_infer_requests_running").Scan(&instance, &deployment); err != nil {
		t.Fatalf("select infer series: %v", err)
	}
	if instance.String != "inst-a" || !instance.Valid || deployment.String != "3" || !deployment.Valid {
		t.Fatalf("infer series columns = instance=%+v deployment=%+v, want inst-a/3", instance, deployment)
	}

	if err := s.db.QueryRow(`SELECT instance, deployment FROM series WHERE metric = ?`,
		metricUtil).Scan(&instance, &deployment); err != nil {
		t.Fatalf("select gpu series: %v", err)
	}
	if instance.Valid || deployment.Valid {
		t.Fatalf("gpu series columns should be NULL, got instance=%+v deployment=%+v", instance, deployment)
	}
}

// TestWriteBatchSelfHealsNullInstanceDeploymentColumns simulates a series row
// left over from an older binary: same labels_key/labels_json an upgraded
// agent would produce, but instance/deployment columns still NULL because
// that binary predates migration 2 (or ran during the rollback window right
// after an upgrade). The next WriteBatch for that exact label set must repair
// the columns in place rather than leaving them permanently invisible to the
// pushed-down matcher.
func TestWriteBatchSelfHealsNullInstanceDeploymentColumns(t *testing.T) {
	store := openTemp(t)
	s := store.(*sqliteStore)

	sm := inferSample("inst-a", "3", 1000, 1)
	key := labelsKey(sm.Metric, sm.Labels)
	lj, err := json.Marshal(sm.Labels)
	if err != nil {
		t.Fatal(err)
	}
	// Insert the row the old way: instance/deployment left NULL.
	if _, err := s.db.Exec(`INSERT INTO series(metric, labels_key, labels_json) VALUES(?, ?, ?)`,
		sm.Metric, key, string(lj)); err != nil {
		t.Fatalf("seed pre-upgrade row: %v", err)
	}
	var seriesID int64
	if err := s.db.QueryRow(`SELECT id FROM series WHERE labels_key = ?`, key).Scan(&seriesID); err != nil {
		t.Fatal(err)
	}
	if _, err := s.db.Exec(`INSERT INTO samples(series_id, ts_ms, value) VALUES(?, ?, ?)`, seriesID, int64(500), 0.0); err != nil {
		t.Fatal(err)
	}

	var instance, deployment sql.NullString
	if err := s.db.QueryRow(`SELECT instance, deployment FROM series WHERE labels_key = ?`, key).
		Scan(&instance, &deployment); err != nil {
		t.Fatal(err)
	}
	if instance.Valid || deployment.Valid {
		t.Fatalf("seed row should start NULL, got instance=%+v deployment=%+v", instance, deployment)
	}

	// The regression this guards: before the ON CONFLICT DO UPDATE fix,
	// WriteBatch used INSERT OR IGNORE, so writing the same label set again
	// left the columns NULL forever and this matcher would find nothing.
	if err := s.WriteBatch([]Sample{sm}); err != nil {
		t.Fatalf("write: %v", err)
	}
	got, err := s.QueryRange(sm.Metric, map[string]string{"deployment": "3"}, 0, 10000, 1000)
	if err != nil {
		t.Fatalf("query: %v", err)
	}
	if len(got) != 1 {
		t.Fatalf("got %d series matching deployment=3 after heal, want 1: %+v", len(got), got)
	}
	if got[0].Labels["instance"] != "inst-a" {
		t.Fatalf("healed series labels = %+v", got[0].Labels)
	}
	// The same series row must have been reused (same id), not duplicated.
	if err := s.db.QueryRow(`SELECT instance, deployment FROM series WHERE id = ?`, seriesID).
		Scan(&instance, &deployment); err != nil {
		t.Fatal(err)
	}
	if instance.String != "inst-a" || deployment.String != "3" {
		t.Fatalf("series id %d not healed in place: instance=%+v deployment=%+v", seriesID, instance, deployment)
	}
	var seriesCount int
	if err := s.db.QueryRow(`SELECT COUNT(*) FROM series WHERE labels_key = ?`, key).Scan(&seriesCount); err != nil {
		t.Fatal(err)
	}
	if seriesCount != 1 {
		t.Fatalf("labels_key %q has %d series rows after heal, want 1 (no duplicate)", key, seriesCount)
	}
}

// TestQueryRangeInstanceDeploymentFilterMatchesUnfilteredScan writes samples
// spanning several instances/deployments (plus one series with neither label)
// under one metric, then checks that filtering by instance and/or deployment
// through the SQL-pushed path returns exactly the same series a full,
// unfiltered scan filtered down to in Go would - proving the indexed path
// changes nothing about the result, only how it is computed.
func TestQueryRangeInstanceDeploymentFilterMatchesUnfilteredScan(t *testing.T) {
	s := openTemp(t)
	metric := "gpuworks_infer_requests_running"
	noLabelInferSample := Sample{
		Metric: metric,
		Labels: map[string]string{"node": "node-1", "engine": "vllm"}, // no instance/deployment at all
		Value:  40, TimeMS: 1000,
	}
	batch := []Sample{
		inferSample("inst-a", "1", 1000, 10),
		inferSample("inst-b", "1", 1000, 20),
		inferSample("inst-c", "2", 1000, 30),
		noLabelInferSample,
		sample("0", 1000, 99), // unrelated metric-shaped series, no instance/deployment
	}
	if err := s.WriteBatch(batch); err != nil {
		t.Fatalf("write: %v", err)
	}

	all, err := s.QueryRange(metric, nil, 0, 10000, 1000)
	if err != nil {
		t.Fatalf("unfiltered query: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("got %d unfiltered series, want 4", len(all))
	}

	cases := []struct {
		name     string
		matchers map[string]string
	}{
		{"instance only", map[string]string{"instance": "inst-a"}},
		{"deployment only", map[string]string{"deployment": "1"}},
		{"instance and deployment", map[string]string{"instance": "inst-b", "deployment": "1"}},
		{"deployment with no match", map[string]string{"deployment": "999"}},
		{"instance plus non-indexed label", map[string]string{"instance": "inst-c", "engine": "vllm"}},
		{"instance plus mismatched non-indexed label", map[string]string{"instance": "inst-c", "engine": "sglang"}},
		// An empty matcher value means "label absent or empty" under
		// matchesAll (labels[k] defaults to "" when the key is missing).
		// "deployment = ''" in SQL is never true for a NULL column, so this
		// must stay a Go-side check rather than being pushed down.
		{"empty deployment value matches the label-less series", map[string]string{"deployment": ""}},
		{"empty instance value matches the label-less series", map[string]string{"instance": ""}},
		{"instance filter with empty deployment value excludes a labeled series", map[string]string{"instance": "inst-a", "deployment": ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			want := manualFilter(all, tc.matchers)
			got, err := s.QueryRange(metric, tc.matchers, 0, 10000, 1000)
			if err != nil {
				t.Fatalf("filtered query: %v", err)
			}
			assertSameSeries(t, got, want)
		})
	}
}

func TestQueryLatestInstanceDeploymentFilterMatchesUnfilteredScan(t *testing.T) {
	s := openTemp(t)
	metric := "gpuworks_infer_requests_running"
	noLabelInferSample := Sample{
		Metric: metric,
		Labels: map[string]string{"node": "node-1", "engine": "vllm"}, // no instance/deployment at all
		Value:  40, TimeMS: 1000,
	}
	batch := []Sample{
		inferSample("inst-a", "1", 1000, 10),
		inferSample("inst-b", "1", 2000, 20),
		inferSample("inst-c", "2", 1500, 30),
		noLabelInferSample,
	}
	if err := s.WriteBatch(batch); err != nil {
		t.Fatalf("write: %v", err)
	}
	all, err := s.QueryLatest(metric, nil, 0)
	if err != nil {
		t.Fatalf("unfiltered latest: %v", err)
	}
	if len(all) != 4 {
		t.Fatalf("got %d unfiltered latest points, want 4", len(all))
	}

	cases := []struct {
		name     string
		matchers map[string]string
	}{
		{"instance only", map[string]string{"instance": "inst-b"}},
		{"deployment only", map[string]string{"deployment": "1"}},
		{"instance and deployment", map[string]string{"instance": "inst-a", "deployment": "1"}},
		{"instance and mismatched deployment", map[string]string{"instance": "inst-a", "deployment": "2"}},
		// Same empty-value semantics as QueryRange: must stay a Go-side
		// check, since "deployment = ''" cannot match a NULL column.
		{"empty deployment value matches the label-less point", map[string]string{"deployment": ""}},
		{"instance filter with empty deployment value excludes a labeled point", map[string]string{"instance": "inst-a", "deployment": ""}},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			var want []LatestPoint
			for _, p := range all {
				if matchesAll(p.Labels, tc.matchers) {
					want = append(want, p)
				}
			}
			got, err := s.QueryLatest(metric, tc.matchers, 0)
			if err != nil {
				t.Fatalf("filtered latest: %v", err)
			}
			if len(got) != len(want) {
				t.Fatalf("got %d points, want %d (got=%+v want=%+v)", len(got), len(want), got, want)
			}
			for _, w := range want {
				found := false
				for _, g := range got {
					if g.Labels["instance"] == w.Labels["instance"] && g.Value == w.Value {
						found = true
						break
					}
				}
				if !found {
					t.Fatalf("missing expected point %+v in %+v", w, got)
				}
			}
		})
	}
}

// manualFilter reproduces the pre-index QueryRange contract: scan every
// series for the metric and keep the ones whose full label set matches every
// matcher, exactly (matchesAll). Used as the ground truth the SQL-pushed path
// must reproduce.
func manualFilter(all []Series, matchers map[string]string) []Series {
	var out []Series
	for _, s := range all {
		if matchesAll(s.Labels, matchers) {
			out = append(out, s)
		}
	}
	return out
}

func assertSameSeries(t *testing.T, got, want []Series) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("got %d series, want %d (got=%+v want=%+v)", len(got), len(want), got, want)
	}
	for _, w := range want {
		found := false
		for _, g := range got {
			if labelsEqual(g.Labels, w.Labels) {
				found = true
				break
			}
		}
		if !found {
			t.Fatalf("missing expected series with labels %v in %+v", w.Labels, got)
		}
	}
}

func labelsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}

// TestRetainDeletesOrphanedSeries checks that once every sample of a series is
// gone, the now-pointless series row is deleted too - otherwise it would keep
// showing up as an empty-points series in QueryRange forever.
func TestRetainDeletesOrphanedSeries(t *testing.T) {
	s := openTemp(t)
	batch := []Sample{
		sample("0", 1000, 1), // this GPU's only sample; will be fully retained away
		sample("1", 1000, 2),
		sample("1", 2000, 3), // survives the cutoff
	}
	if err := s.WriteBatch(batch); err != nil {
		t.Fatalf("write: %v", err)
	}
	before, err := s.QueryRange(metricUtil, nil, 0, 10000, 1000)
	if err != nil {
		t.Fatalf("query before retain: %v", err)
	}
	if len(before) != 2 {
		t.Fatalf("got %d series before retain, want 2", len(before))
	}

	if err := s.Retain(context.Background(), 2000); err != nil {
		t.Fatalf("retain: %v", err)
	}

	after, err := s.QueryRange(metricUtil, nil, 0, 10000, 1000)
	if err != nil {
		t.Fatalf("query after retain: %v", err)
	}
	if len(after) != 1 {
		t.Fatalf("got %d series after retain, want 1 (orphan gpu=0 series should be gone): %+v", len(after), after)
	}
	if after[0].Labels["gpu"] != "1" {
		t.Fatalf("surviving series = %+v, want gpu=1", after[0])
	}

	// The row-level check: the orphan's series row itself must be gone, not
	// merely unreachable through the label-matching path.
	sq := s.(*sqliteStore)
	var count int
	if err := sq.db.QueryRow(`SELECT COUNT(*) FROM series WHERE metric = ?`, metricUtil).Scan(&count); err != nil {
		t.Fatalf("count series rows: %v", err)
	}
	if count != 1 {
		t.Fatalf("series table has %d rows for %s after retain, want 1", count, metricUtil)
	}
}

func TestBenchmarkProcessMetricRoundTrip(t *testing.T) {
	s := openTemp(t)
	labels := map[string]string{"benchmark": "7", "point": "2", "node": "server"}
	if err := s.WriteBatch([]Sample{{Metric: "gpuworks_bench_ttft_p50_ms", Labels: labels, Value: 11, TimeMS: 1000}, {Metric: "gpuworks_bench_ttft_p50_ms", Labels: labels, Value: 13, TimeMS: 2500}}); err != nil {
		t.Fatal(err)
	}
	got, err := s.QueryRange("gpuworks_bench_ttft_p50_ms", map[string]string{"benchmark": "7", "point": "2"}, 0, 5000, 1000)
	if err != nil || len(got) != 1 || len(got[0].Points) != 2 {
		t.Fatalf("got=%+v err=%v", got, err)
	}
	if got[0].Labels["node"] != "server" || got[0].Points[1].Value != 13 {
		t.Fatalf("unexpected benchmark series: %+v", got[0])
	}
}
