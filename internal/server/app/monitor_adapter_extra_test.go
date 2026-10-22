package app

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// This file continues monitor_adapter_test.go (which holds the shared
// fixture builders and the basic happy-path/kv_saturated end-to-end tests):
// the not-found seam, headless-worker exclusion, chart label vocabulary,
// query-count and cancellation-survival tests.

// TestMonitorServiceDeploymentNotFoundAcrossTheSeam proves the exact
// sentinel api.go's handler checks with errors.Is reaches the caller
// unchanged, both for an id that never existed and for one that did but was
// deleted since.
func TestMonitorServiceDeploymentNotFoundAcrossTheSeam(t *testing.T) {
	fx := buildMonitorFixture(t)
	svc := newMonitorAPIService(fx.st, fx.ts, func() time.Time { return fx.now }, nil, nil)

	if _, err := svc.Deployment(context.Background(), fx.dep.ID+999, time.Hour); !errors.Is(err, api.ErrMonitorDeploymentNotFound) {
		t.Fatalf("err = %v, want errors.Is(err, api.ErrMonitorDeploymentNotFound) for a never-existed id", err)
	}

	if err := fx.st.DeleteDeployment(fx.dep.ID); err != nil {
		t.Fatalf("DeleteDeployment: %v", err)
	}
	if _, err := svc.Deployment(context.Background(), fx.dep.ID, time.Hour); !errors.Is(err, api.ErrMonitorDeploymentNotFound) {
		t.Fatalf("err = %v, want errors.Is(err, api.ErrMonitorDeploymentNotFound) for a deleted deployment", err)
	}
}

// TestMonitorServiceHeadlessFollowerExcluded proves a distributed
// deployment's headless follower rank must not inflate the ready count or
// get its own replica row, all the way from the store's desired-state JSON
// through to the wire dto: a follower is a process-group participant with
// no API endpoint of its own, not a replica.
func TestMonitorServiceHeadlessFollowerExcluded(t *testing.T) {
	fx := newMonitorFixtureBase(t) // one leader instance already created
	followerSpec := proto.InstanceDesiredState{
		UUID: "monitor-follower", DeploymentID: fx.dep.ID, Engine: "vllm", DesiredState: "running",
		Headless: true, DistRole: "follower",
	}
	followerJSON, err := json.Marshal(followerSpec)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := fx.st.CreateInstance(&store.Instance{
		UUID: followerSpec.UUID, DeploymentID: fx.dep.ID, NodeID: fx.node.ID, State: proto.InstRunning,
		DesiredStateJSON: string(followerJSON),
	}); err != nil {
		t.Fatalf("create follower instance: %v", err)
	}

	svc := newMonitorAPIService(fx.st, fx.ts, func() time.Time { return fx.now }, nil, nil)

	overview, err := svc.Overview(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if len(overview.Services) != 1 || overview.Services[0].Replicas.Ready != 1 || overview.Services[0].Replicas.Desired != 1 {
		t.Fatalf("overview replicas = %+v, want ready=1 desired=1 (the headless follower must not count)", overview.Services[0].Replicas)
	}

	deployment, err := svc.Deployment(context.Background(), fx.dep.ID, time.Hour)
	if err != nil {
		t.Fatalf("Deployment: %v", err)
	}
	if deployment.Deployment.Replicas.Ready != 1 {
		t.Fatalf("deployment.replicas.ready = %d, want 1", deployment.Deployment.Replicas.Ready)
	}
	if len(deployment.Replicas) != 1 || deployment.Replicas[0].InstanceUUID != fx.inst.UUID {
		t.Fatalf("replicas = %+v, want exactly the one leader row", deployment.Replicas)
	}
}

// TestMonitorServiceDeploymentChartLabelVocabularyMatchesUIFixture parses
// ui/tests/fixtures/monitor-deployment.json with DisallowUnknownFields (so
// a struct-shape drift between dto.MonitorDeployment and the fixture fails
// loudly here rather than only in the UI's own tests), then checks the
// server's own chart label vocabulary - the set of labels.metric values and
// whether "instance" is present - for concurrency/kv/throughput against
// that fixture's.
func TestMonitorServiceDeploymentChartLabelVocabularyMatchesUIFixture(t *testing.T) {
	repoRoot, err := filepath.Abs(filepath.Join("..", "..", ".."))
	if err != nil {
		t.Fatal(err)
	}
	fixturePath := filepath.Join(repoRoot, "ui", "tests", "fixtures", "monitor-deployment.json")
	raw, err := os.ReadFile(fixturePath)
	if err != nil {
		t.Fatalf("read %s: %v (is the fixture path still correct?)", fixturePath, err)
	}
	var fixture dto.MonitorDeployment
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.DisallowUnknownFields()
	if err := dec.Decode(&fixture); err != nil {
		t.Fatalf("decode %s into dto.MonitorDeployment (DisallowUnknownFields): %v", fixturePath, err)
	}

	fx := newMonitorFixtureBase(t)
	depLabels := map[string]string{"deployment": strconv.FormatUint(uint64(fx.dep.ID), 10)}
	writeMonitorSample(t, fx.ts, "gpuworks_infer_requests_running", depLabels, 4, fx.now)
	writeMonitorSample(t, fx.ts, "gpuworks_infer_requests_waiting", depLabels, 1, fx.now)
	writeMonitorSample(t, fx.ts, "gpuworks_infer_gpu_cache_usage_percent", depLabels, 0.5, fx.now)
	writeMonitorSample(t, fx.ts, "gpuworks_infer_kv_hit_rate_instant", depLabels, 0.4, fx.now)
	writeMonitorSample(t, fx.ts, "gpuworks_infer_prompt_tokens_total", depLabels, 100, fx.now.Add(-time.Second))
	writeMonitorSample(t, fx.ts, "gpuworks_infer_prompt_tokens_total", depLabels, 200, fx.now)
	writeMonitorSample(t, fx.ts, "gpuworks_infer_generation_tokens_total", depLabels, 50, fx.now.Add(-time.Second))
	writeMonitorSample(t, fx.ts, "gpuworks_infer_generation_tokens_total", depLabels, 90, fx.now)

	svc := newMonitorAPIService(fx.st, fx.ts, func() time.Time { return fx.now }, nil, nil)
	server, err := svc.Deployment(context.Background(), fx.dep.ID, time.Hour)
	if err != nil {
		t.Fatalf("Deployment: %v", err)
	}

	for _, group := range []string{"concurrency", "kv", "throughput"} {
		serverVocab := chartMetricVocab(chartGroup(server.Charts, group))
		fixtureVocab := chartMetricVocab(chartGroup(fixture.Charts, group))
		if !equalStringSets(serverVocab, fixtureVocab) {
			t.Fatalf("charts.%s labels.metric vocabulary = %v, want the fixture's %v", group, serverVocab, fixtureVocab)
		}
		if !chartHasInstanceLabel(chartGroup(server.Charts, group)) {
			t.Fatalf("charts.%s must label every series with \"instance\"", group)
		}
		if !chartHasInstanceLabel(chartGroup(fixture.Charts, group)) {
			t.Fatalf("fixture charts.%s unexpectedly has no \"instance\" label - fixture drifted from the contract this test pins", group)
		}
	}
}

func chartGroup(charts dto.MonitorCharts, name string) []dto.MonitorSeries {
	switch name {
	case "latency":
		return charts.Latency
	case "concurrency":
		return charts.Concurrency
	case "kv":
		return charts.KV
	case "throughput":
		return charts.Throughput
	case "stage_time":
		return charts.StageTime
	case "preemptions":
		return charts.Preemptions
	default:
		return nil
	}
}

func chartMetricVocab(series []dto.MonitorSeries) []string {
	set := map[string]bool{}
	for _, s := range series {
		set[s.Labels["metric"]] = true
	}
	out := make([]string, 0, len(set))
	for k := range set {
		out = append(out, k)
	}
	sort.Strings(out)
	return out
}

func chartHasInstanceLabel(series []dto.MonitorSeries) bool {
	if len(series) == 0 {
		return true // nothing to check; an empty group trivially satisfies this
	}
	for _, s := range series {
		if _, ok := s.Labels["instance"]; !ok {
			return false
		}
	}
	return true
}

func equalStringSets(a, b []string) bool {
	if len(a) != len(b) {
		return false
	}
	for i := range a {
		if a[i] != b[i] {
			return false
		}
	}
	return true
}

// countingTSDB wraps a real tsdb.Store and counts QueryRange calls, so a
// "no doubled ComputeWindow" claim can be checked directly instead of
// inferred from timing.
type countingTSDB struct {
	tsdb.Store
	queryRangeCalls int64
}

func (c *countingTSDB) QueryRange(metric string, matchers map[string]string, startMS, endMS, stepMS int64) ([]tsdb.Series, error) {
	atomic.AddInt64(&c.queryRangeCalls, 1)
	return c.Store.QueryRange(metric, matchers, startMS, endMS, stepMS)
}

// TestMonitorServiceOverviewDoesNotRecomputeWindowPerService proves the
// overview reads ServiceRow.HasRunning/RunningAvg directly (BuildCluster
// already computed them) rather than calling monitor.ComputeWindow a second
// time per deployment - which would double the tsdb query count for every
// deployment in the overview.
func TestMonitorServiceOverviewDoesNotRecomputeWindowPerService(t *testing.T) {
	fx := buildMonitorFixture(t)
	counting := &countingTSDB{Store: fx.ts}
	svc := newMonitorAPIService(fx.st, counting, func() time.Time { return fx.now }, nil, nil)

	if _, err := svc.Overview(context.Background(), time.Hour); err != nil {
		t.Fatalf("Overview: %v", err)
	}
	firstCallCount := atomic.LoadInt64(&counting.queryRangeCalls)
	if firstCallCount == 0 {
		t.Fatal("expected at least some QueryRange calls")
	}

	// A second, freshly-cached Overview computation (past the entry we just
	// populated - use a distinct range so it isn't served from cache) for
	// the SAME single deployment must cost the same number of queries, not
	// double: if buildOverview still called ComputeWindow a second time per
	// service, this would grow with the deployment count for reasons
	// unrelated to any new deployment appearing.
	if _, err := svc.Overview(context.Background(), 6*time.Hour); err != nil {
		t.Fatalf("Overview (6h): %v", err)
	}
	secondCallCount := atomic.LoadInt64(&counting.queryRangeCalls) - firstCallCount
	if secondCallCount != firstCallCount {
		t.Fatalf("QueryRange calls for a second, differently-ranged overview = %d, want exactly %d (same deployment, same query shape - no per-service ComputeWindow duplication)", secondCallCount, firstCallCount)
	}
}

// TestMonitorServiceOverviewConcurrentCallsShareOneComputation proves many
// browsers polling the same overview at once cost exactly one computation
// (monitor.Cached's single-flight), not one per caller.
func TestMonitorServiceOverviewConcurrentCallsShareOneComputation(t *testing.T) {
	fx := buildMonitorFixture(t)
	counting := &countingTSDB{Store: fx.ts}
	svc := newMonitorAPIService(fx.st, counting, func() time.Time { return fx.now }, nil, nil)

	const n = 8
	var wg sync.WaitGroup
	wg.Add(n)
	errs := make([]error, n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			_, err := svc.Overview(context.Background(), time.Hour)
			errs[i] = err
		}(i)
	}
	wg.Wait()
	for i, err := range errs {
		if err != nil {
			t.Fatalf("Overview[%d]: %v", i, err)
		}
	}

	// Establish the per-computation query count with one solo call against
	// an unshared cache key (a distinct range), then compare.
	solo := &countingTSDB{Store: fx.ts}
	soloSvc := newMonitorAPIService(fx.st, solo, func() time.Time { return fx.now }, nil, nil)
	if _, err := soloSvc.Overview(context.Background(), 24*time.Hour); err != nil {
		t.Fatalf("solo Overview: %v", err)
	}
	perComputation := atomic.LoadInt64(&solo.queryRangeCalls)

	got := atomic.LoadInt64(&counting.queryRangeCalls)
	if got != perComputation {
		t.Fatalf("QueryRange calls for %d concurrent overviews = %d, want exactly %d (one shared computation)", n, got, perComputation)
	}
}

// TestMonitorServiceOverviewCancelledCallStillSucceeds is the end-to-end
// proof through the real store-backed adapter: a call whose context is
// already canceled before Overview even starts (the same shape as an
// aborted XHR canceling a context.Context.WithCancel handed down from
// net/http) must not fail, and a subsequent fresh call for the same key
// must not inherit a cached failure either. See
// monitorapi.TestOverviewSurvivesLeaderCancellation for the concurrent
// leader/waiter race this same fix covers at the Service level.
func TestMonitorServiceOverviewCancelledCallStillSucceeds(t *testing.T) {
	fx := buildMonitorFixture(t)
	svc := newMonitorAPIService(fx.st, fx.ts, func() time.Time { return fx.now }, nil, nil)

	cancelledCtx, cancel := context.WithCancel(context.Background())
	cancel()
	first, err := svc.Overview(cancelledCtx, time.Hour)
	if err != nil {
		t.Fatalf("Overview with an already-canceled context: %v, want success (compute runs under context.WithoutCancel)", err)
	}
	if len(first.Services) != 1 {
		t.Fatalf("first result = %+v, want the one deployment", first.Services)
	}

	second, err := svc.Overview(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("second Overview (fresh context): %v, want the cached success, not a poisoned failure", err)
	}
	if len(second.Services) != 1 {
		t.Fatalf("second result = %+v", second.Services)
	}
}

func TestMonitorServiceSeriesGroupsByNode(t *testing.T) {
	fx := buildMonitorFixture(t)
	svc := newMonitorAPIService(fx.st, fx.ts, func() time.Time { return fx.now }, nil, nil)

	out, err := svc.Series(context.Background(), "gpuworks_gpu_memory_used_bytes", "node", "avg", time.Hour, 30*time.Second)
	if err != nil {
		t.Fatalf("Series: %v", err)
	}
	if len(out) != 1 || out[0].Labels["node"] != fx.nodeUUID {
		t.Fatalf("series = %+v, want one series grouped by node %q", out, fx.nodeUUID)
	}
	if len(out[0].Points) == 0 {
		t.Fatalf("series points = %+v", out[0].Points)
	}

	empty, err := svc.Series(context.Background(), "gpuworks_does_not_exist", "", "avg", time.Hour, 30*time.Second)
	if err != nil {
		t.Fatalf("Series (unknown metric): %v", err)
	}
	if len(empty) != 0 {
		t.Fatalf("unknown metric must return no series, got %+v", empty)
	}
}
