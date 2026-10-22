package monitor

import (
	"context"
	"testing"
	"time"

	svmonitor "gpuworks.ai/gpuworks/internal/server/monitor"
)

// TestClusterOverviewQueryCountStaysConstantAsDeploymentsScale proves the
// plan's second-phase capacity requirement
// (artifacts/archive/2026-09-19-monitor-center/plan.md §8: "监控总览每指标一次全集群
// 查询，Go 里按部署分组；不再按部署逐个查") holds in the real code, not just in
// window_fetch.go's doc comments: BuildCluster fetches every metric
// fleet-wide exactly once (FetchFleetWindowSeries: one nil-matcher
// QueryRange per metric name - 15 engine series, 4 proxy accounting series
// and the 8 gateway series that stand in for engine ones) plus the cluster
// token-rate QueryRange and 4 QueryLatest probes (vramByNode's, and one
// range-presence probe each for proxy requests, TTFT and TPOT) - a fixed
// total of 32 independent of how many deployments the fleet has. The GPU
// temperature, utilization, memory-bandwidth and throttle-reason and node
// CPU window queries (5 more, 37 in total before) were removed together with
// the diagnosis rules that read them. A regression that reintroduced a
// per-deployment tsdb query (the O(deployments x metrics) shape the plan
// explicitly rules out) would make this grow with N and fail here.
func TestClusterOverviewQueryCountStaysConstantAsDeploymentsScale(t *testing.T) {
	now := time.Date(2026, 9, 19, 12, 0, 0, 0, time.UTC)

	buildInventory := func(n int) svmonitor.Inventory {
		var inv svmonitor.Inventory
		for i := 1; i <= n; i++ {
			one := singleInstanceInventory(uint(i), 1, 1)
			inv.Deployments = append(inv.Deployments, one.Deployments...)
			inv.Instances = append(inv.Instances, one.Instances...)
		}
		inv.Nodes = []svmonitor.NodeRef{{UUID: "n1", Name: "node-1", State: "online"}}
		inv.GPUs = []svmonitor.GPURef{{NodeUUID: "n1", Index: 0, MemoryTotalBytes: 80 << 30, Healthy: true}}
		return inv
	}

	callsFor := func(t *testing.T, n int) int {
		t.Helper()
		store := newStore(t)
		counting := &countingStore{Store: store}
		for i := 1; i <= n; i++ {
			writeGaugeHold(t, store, "gpuworks_infer_requests_running", map[string]string{"deployment": depKey(uint(i))}, 5, now)
		}
		deps := svmonitor.Deps{
			TSDB: counting, Now: func() time.Time { return now },
			Inventory: func(context.Context) (svmonitor.Inventory, error) { return buildInventory(n), nil },
		}
		if _, err := svmonitor.BuildCluster(context.Background(), deps, time.Hour); err != nil {
			t.Fatalf("BuildCluster(n=%d): %v", n, err)
		}
		return counting.totalCalls()
	}

	callsFor1 := callsFor(t, 1)
	callsFor20 := callsFor(t, 20)
	if callsFor1 == 0 {
		t.Fatal("expected at least one tsdb call for a single-deployment overview")
	}
	const wantCalls = 32 // 28 QueryRange + 4 QueryLatest, see the doc comment above
	if callsFor1 != wantCalls {
		t.Fatalf("tsdb query count for a 1-deployment overview = %d, want %d", callsFor1, wantCalls)
	}
	if callsFor1 != callsFor20 {
		t.Fatalf("tsdb query count grew with deployment count: 1 deployment = %d calls, 20 deployments = %d calls; want equal (O(metrics), not O(deployments))", callsFor1, callsFor20)
	}
}
