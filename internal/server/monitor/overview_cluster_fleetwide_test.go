package monitor

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// TestBuildClusterQueryCountIsIndependentOfDeploymentCount is the
// plan §8 second-phase capacity requirement's own test: BuildCluster must
// query tsdb once per metric for the whole fleet, not once per metric per
// deployment. A cluster of 1 deployment and one of 50 must therefore issue
// exactly the same number of QueryRange calls. The total is also asserted
// to be small (<= 28): the fixed per-snapshot metric count the phase2
// capacity review targets, regardless of fleet size.
func TestBuildClusterQueryCountIsIndependentOfDeploymentCount(t *testing.T) {
	countQueries := func(n int) int {
		store := newTestStore(t)
		recorder := &queryRecordingStore{Store: store}
		var deployments []DeploymentRef
		var instances []InstanceRef
		for i := 1; i <= n; i++ {
			id := uint(i)
			deployments = append(deployments, DeploymentRef{ID: id, Name: "svc", ReplicasDesired: 1})
			uuid := depKeyString(id) + "-inst"
			instances = append(instances, InstanceRef{UUID: uuid, DeploymentID: id, NodeUUID: "n1", GPUIndexes: []int{0}})
			labels := map[string]string{"instance": uuid, "deployment": depKeyString(id)}
			writeSample(t, store, "gpuworks_infer_requests_running", labels, 5, baseTime)
		}
		inv := Inventory{Deployments: deployments, Instances: instances, Nodes: []NodeRef{{UUID: "n1", Name: "n1"}}}
		deps := Deps{TSDB: recorder, Inventory: inventoryFunc(inv), Now: func() time.Time { return baseTime }}

		if _, err := BuildCluster(context.Background(), deps, time.Hour); err != nil {
			t.Fatalf("BuildCluster(n=%d): %v", n, err)
		}
		return len(recorder.calls)
	}

	one := countQueries(1)
	fifty := countQueries(50)
	if one != fifty {
		t.Fatalf("query count for 1 deployment = %d, for 50 deployments = %d; want equal (O(metrics), not O(deployments))", one, fifty)
	}
	// 15 engine series, 4 gateway request series and the cluster token-rate
	// chart query, plus the 8 gateway series that stand in for an engine's
	// (window_sources.go) = 28. This was 33 before the GPU temperature,
	// utilization, memory-bandwidth and throttle-reason and node CPU window
	// queries were removed together with the diagnosis rules that read them.
	const maxQueries = 28
	if one > maxQueries {
		t.Errorf("query count = %d, want <= %d fixed metrics per snapshot", one, maxQueries)
	}
	t.Logf("BuildCluster issued %d QueryRange calls regardless of deployment count (1 vs 50)", one)
}

// TestBuildClusterRangePresenceProbeIsFleetWideNotPerDeployment proves the
// "collected somewhere in the wider chart range" check BuildCluster added
// for request_rate/error_rate/ttft/tpot (ClusterSignals.ProxyNoRecent/
// TTFTNoRecent/TPOTNoRecent) costs exactly one QueryLatest call per metric
// family (proxy_requests_total, ttft_seconds, tpot_seconds - 3 calls) on top
// of the one QueryLatest vramByNode already issued (gpuworks_gpu_memory_
// used_bytes - see overview_cluster_signals.go), for 4 total, regardless of
// fleet size - never once per deployment - and that the pre-existing
// QueryRange count (asserted by
// TestBuildClusterQueryCountIsIndependentOfDeploymentCount above) is
// completely unaffected, since the new probes use a different Store method.
func TestBuildClusterRangePresenceProbeIsFleetWideNotPerDeployment(t *testing.T) {
	countCalls := func(n int) (queryRangeCalls, queryLatestCalls int) {
		store := newTestStore(t)
		recorder := &queryRecordingStore{Store: store}
		var deployments []DeploymentRef
		var instances []InstanceRef
		for i := 1; i <= n; i++ {
			id := uint(i)
			deployments = append(deployments, DeploymentRef{ID: id, Name: "svc", ReplicasDesired: 1})
			uuid := depKeyString(id) + "-inst"
			instances = append(instances, InstanceRef{UUID: uuid, DeploymentID: id, NodeUUID: "n1", GPUIndexes: []int{0}})
			labels := map[string]string{"instance": uuid, "deployment": depKeyString(id)}
			writeSample(t, store, "gpuworks_infer_requests_running", labels, 5, baseTime)
		}
		inv := Inventory{Deployments: deployments, Instances: instances, Nodes: []NodeRef{{UUID: "n1", Name: "n1"}}}
		deps := Deps{TSDB: recorder, Inventory: inventoryFunc(inv), Now: func() time.Time { return baseTime }}

		if _, err := BuildCluster(context.Background(), deps, time.Hour); err != nil {
			t.Fatalf("BuildCluster(n=%d): %v", n, err)
		}
		return len(recorder.calls), len(recorder.latestCalls)
	}

	oneRange, oneLatest := countCalls(1)
	fiftyRange, fiftyLatest := countCalls(50)

	if oneLatest != fiftyLatest {
		t.Fatalf("QueryLatest calls for 1 deployment = %d, for 50 = %d; want equal (one probe per metric family, not per deployment)", oneLatest, fiftyLatest)
	}
	const wantLatestCalls = 4 // proxy_requests_total, ttft_seconds, tpot_seconds + vramByNode's own probe
	if oneLatest != wantLatestCalls {
		t.Fatalf("QueryLatest calls = %d, want exactly %d (one per widened-range metric family)", oneLatest, wantLatestCalls)
	}
	if oneRange != fiftyRange {
		t.Fatalf("QueryRange calls for 1 deployment = %d, for 50 = %d; want equal and unaffected by the new QueryLatest probes", oneRange, fiftyRange)
	}
	t.Logf("BuildCluster issued %d QueryRange + %d QueryLatest calls regardless of deployment count (1 vs 50)", oneRange, oneLatest)
}

// TestBuildFleetWindowIndexIsBuiltOnceNotPerDeployment is item 9's coverage:
// BuildCluster must index every fleet-wide series exactly once per
// snapshot (BuildFleetWindowIndex), then reuse that index for every
// deployment's GroupWindowSeries call - never re-index the same fleet
// series once per deployment.
func TestBuildFleetWindowIndexIsBuiltOnceNotPerDeployment(t *testing.T) {
	countIndexBuilds := func(n int) int64 {
		store := newTestStore(t)
		var deployments []DeploymentRef
		var instances []InstanceRef
		for i := 1; i <= n; i++ {
			id := uint(i)
			deployments = append(deployments, DeploymentRef{ID: id, Name: "svc"})
			uuid := depKeyString(id) + "-inst"
			instances = append(instances, InstanceRef{UUID: uuid, DeploymentID: id})
			labels := map[string]string{"instance": uuid, "deployment": depKeyString(id)}
			writeSample(t, store, "gpuworks_infer_requests_running", labels, 5, baseTime)
		}
		inv := Inventory{Deployments: deployments, Instances: instances}
		deps := Deps{TSDB: store, Inventory: inventoryFunc(inv), Now: func() time.Time { return baseTime }}

		before := atomic.LoadInt64(&indexBuildCount)
		if _, err := BuildCluster(context.Background(), deps, time.Hour); err != nil {
			t.Fatalf("BuildCluster(n=%d): %v", n, err)
		}
		return atomic.LoadInt64(&indexBuildCount) - before
	}

	one := countIndexBuilds(1)
	fifty := countIndexBuilds(50)
	if one == 0 {
		t.Fatal("expected at least one index build for a non-empty fleet")
	}
	if one != fifty {
		t.Fatalf("index builds for 1 deployment = %d, for 50 deployments = %d; want equal (indexed once per snapshot, not once per deployment)", one, fifty)
	}
	t.Logf("BuildCluster built %d indexes regardless of deployment count (1 vs 50)", one)
}

// TestBuildClusterPhase2SignalSparklinesComeFromAlreadyFetchedFleetData is
// item 4/5's coverage: error_rate/request_rate/preemptions/ttft/tpot all
// get a non-empty, DiagnosisWindow-bucketed sparkline series, and computing
// them costs zero additional QueryRange calls beyond the fixed fleet-wide
// fetch.
func TestBuildClusterPhase2SignalSparklinesComeFromAlreadyFetchedFleetData(t *testing.T) {
	store := newTestStore(t)
	recorder := &queryRecordingStore{Store: store}
	inv := Inventory{
		Deployments: []DeploymentRef{{ID: 1, Name: "svc"}},
		Instances:   []InstanceRef{{UUID: "i1", DeploymentID: 1}},
	}
	deps := Deps{TSDB: recorder, Inventory: inventoryFunc(inv), Now: func() time.Time { return baseTime }}

	start := baseTime.Add(-70 * time.Second)
	for i := 0; i < 8; i++ {
		at := start.Add(time.Duration(i) * 10 * time.Second)
		n := float64(i + 1)
		writeSample(t, store, "gpuworks_proxy_requests_total", map[string]string{"deployment": "1", "class": "2xx"}, 90*n, at)
		writeSample(t, store, "gpuworks_proxy_requests_total", map[string]string{"deployment": "1", "class": "5xx"}, 10*n, at)
		writeSample(t, store, "gpuworks_infer_preemptions_total", map[string]string{"deployment": "1", "instance": "i1"}, n, at)
		writeSample(t, store, "gpuworks_infer_ttft_seconds", map[string]string{"deployment": "1", "instance": "i1", "quantile": "0.95"}, 2.5, at)
		writeSample(t, store, "gpuworks_infer_tpot_seconds", map[string]string{"deployment": "1", "instance": "i1", "quantile": "0.95"}, 0.08, at)
	}

	before := len(recorder.calls)
	snap, err := BuildCluster(context.Background(), deps, time.Hour)
	if err != nil {
		t.Fatalf("BuildCluster: %v", err)
	}
	queriesUsed := len(recorder.calls) - before

	for name, series := range map[string][]tsdb.Point{
		"RequestsPerSecSeries":    snap.Signals.RequestsPerSecSeries,
		"ErrorRateSeries":         snap.Signals.ErrorRateSeries,
		"PreemptionsPerSecSeries": snap.Signals.PreemptionsPerSecSeries,
		"TTFTP95Series":           snap.Signals.TTFTP95Series,
		"TPOTP95Series":           snap.Signals.TPOTP95Series,
	} {
		if len(series) == 0 {
			t.Errorf("Signals.%s = empty, want a non-empty DiagnosisWindow-bucketed series", name)
		}
	}
	// 28 fixed queries (see TestBuildClusterQueryCountIsIndependentOfDeploymentCount;
	// 33 before the GPU temperature/utilization/memory-bandwidth/throttle and
	// node CPU window queries were removed with the diagnosis rules).
	if queriesUsed > 28 {
		t.Errorf("BuildCluster issued %d queries, want <= 28 (sparklines must cost zero extra queries)", queriesUsed)
	}
	t.Logf("queries used = %d", queriesUsed)
}
