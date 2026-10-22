package monitor

import (
	"context"
	"errors"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

func inventoryFunc(inv Inventory) func(context.Context) (Inventory, error) {
	return func(context.Context) (Inventory, error) { return inv, nil }
}

func TestBuildClusterAggregatesReadyReplicasAndVRAM(t *testing.T) {
	store := newTestStore(t)
	inv := Inventory{
		Deployments: []DeploymentRef{
			{ID: 1, Name: "svc-a", Engine: "vllm", ReplicasDesired: 2},
			{ID: 2, Name: "svc-b", Engine: "sglang", ReplicasDesired: 1},
		},
		Instances: []InstanceRef{
			{UUID: "a1", DeploymentID: 1, NodeUUID: "node-1", State: proto.InstRunning},
			{UUID: "a2", DeploymentID: 1, NodeUUID: "node-1", State: proto.InstFailed},
			{UUID: "b1", DeploymentID: 2, NodeUUID: "node-1", State: proto.InstRunning},
		},
		Nodes: []NodeRef{{UUID: "node-1", Name: "node-1-name", State: "online"}},
		GPUs: []GPURef{
			{NodeUUID: "node-1", Index: 0, MemoryTotalBytes: 80 << 30, AllocatedBytes: 40 << 30, Healthy: true},
			{NodeUUID: "node-1", Index: 1, MemoryTotalBytes: 80 << 30, AllocatedBytes: 0, Healthy: true},
		},
	}
	deps := Deps{TSDB: store, Inventory: inventoryFunc(inv), Now: func() time.Time { return baseTime }}

	snap, err := BuildCluster(context.Background(), deps, time.Hour)
	if err != nil {
		t.Fatalf("BuildCluster: %v", err)
	}

	if snap.Signals.ReadyReplicas != 2 { // a1 running + b1 running
		t.Errorf("ReadyReplicas = %d, want 2", snap.Signals.ReadyReplicas)
	}
	if snap.Signals.DesiredReplicas != 3 {
		t.Errorf("DesiredReplicas = %d, want 3", snap.Signals.DesiredReplicas)
	}
	if len(snap.Signals.NotReadyServices) != 1 || snap.Signals.NotReadyServices[0] != "svc-a" {
		t.Errorf("NotReadyServices = %v, want [svc-a]", snap.Signals.NotReadyServices)
	}
	if len(snap.Services) != 2 {
		t.Fatalf("Services = %d, want 2", len(snap.Services))
	}

	if len(snap.VRAMByNode) != 1 {
		t.Fatalf("VRAMByNode = %d rows, want 1", len(snap.VRAMByNode))
	}
	node := snap.VRAMByNode[0]
	if node.TotalBytes != 160<<30 || node.AllocatedBytes != 40<<30 {
		t.Errorf("VRAM node = %+v, want total=160GiB allocated=40GiB", node)
	}
	if len(snap.Events) != 0 {
		t.Errorf("Events = %v, want empty (no events wiring yet)", snap.Events)
	}
	if snap.Signals.HasErrRate {
		t.Error("HasErrRate should be false: no proxy accounting samples were written")
	}
}

// TestBuildClusterServiceRowCarriesRunningFromTheSameWindow proves
// ServiceRow.HasRunning/RunningAvg come from the same Window BuildCluster
// already computed for the snapshot, so a caller never needs a second
// ComputeWindow call (and its tsdb queries) just to learn a deployment's
// average running-request count.
func TestBuildClusterServiceRowCarriesRunningFromTheSameWindow(t *testing.T) {
	store := newTestStore(t)
	inv := Inventory{
		Deployments: []DeploymentRef{{ID: 1, Name: "svc", ReplicasDesired: 1}},
		Instances:   []InstanceRef{{UUID: "i1", DeploymentID: 1, State: proto.InstRunning}},
	}
	deps := Deps{TSDB: store, Inventory: inventoryFunc(inv), Now: func() time.Time { return baseTime }}
	writeSample(t, store, "gpuworks_infer_requests_running", map[string]string{"instance": "i1", "deployment": "1"}, 17, baseTime)

	snap, err := BuildCluster(context.Background(), deps, time.Hour)
	if err != nil {
		t.Fatalf("BuildCluster: %v", err)
	}
	if len(snap.Services) != 1 {
		t.Fatalf("Services = %d, want 1", len(snap.Services))
	}
	row := snap.Services[0]
	if !row.HasRunning || row.RunningAvg != 17 {
		t.Fatalf("HasRunning/RunningAvg = %v/%v, want true/17", row.HasRunning, row.RunningAvg)
	}
}

// TestBuildClusterExcludesHeadlessInstancesFromReadyCount proves a
// distributed deployment's headless follower ranks must not inflate
// ready/desired-adjacent counts: they are process-group participants with
// no endpoint of their own, not replicas. One leader replica plus one
// headless follower must read as ready=1, not 2.
func TestBuildClusterExcludesHeadlessInstancesFromReadyCount(t *testing.T) {
	store := newTestStore(t)
	inv := Inventory{
		Deployments: []DeploymentRef{{ID: 1, Name: "svc", ReplicasDesired: 1}},
		Instances: []InstanceRef{
			{UUID: "leader", DeploymentID: 1, NodeUUID: "node-1", State: proto.InstRunning},
			{UUID: "follower", DeploymentID: 1, NodeUUID: "node-1", State: proto.InstRunning, Headless: true},
		},
	}
	deps := Deps{TSDB: store, Inventory: inventoryFunc(inv), Now: func() time.Time { return baseTime }}

	snap, err := BuildCluster(context.Background(), deps, time.Hour)
	if err != nil {
		t.Fatalf("BuildCluster: %v", err)
	}
	if len(snap.Services) != 1 {
		t.Fatalf("Services = %d, want 1", len(snap.Services))
	}
	if snap.Services[0].ReplicasReady != 1 {
		t.Fatalf("ReplicasReady = %d, want 1 (the headless follower must not count)", snap.Services[0].ReplicasReady)
	}
	if snap.Signals.ReadyReplicas != 1 {
		t.Fatalf("Signals.ReadyReplicas = %d, want 1", snap.Signals.ReadyReplicas)
	}
}

func TestBuildClusterUsesVRAMUsedFromLatestGPUMemory(t *testing.T) {
	store := newTestStore(t)
	inv := Inventory{
		Nodes: []NodeRef{{UUID: "node-1", Name: "n1"}},
		GPUs:  []GPURef{{NodeUUID: "node-1", Index: 0, MemoryTotalBytes: 80 << 30}},
	}
	deps := Deps{TSDB: store, Inventory: inventoryFunc(inv), Now: func() time.Time { return baseTime }}
	writeSample(t, store, "gpuworks_gpu_memory_used_bytes", map[string]string{"node": "node-1", "gpu": "0"}, float64(20<<30), baseTime.Add(-time.Minute))

	snap, err := BuildCluster(context.Background(), deps, time.Hour)
	if err != nil {
		t.Fatalf("BuildCluster: %v", err)
	}
	if len(snap.VRAMByNode) != 1 || !snap.VRAMByNode[0].HasUsedBytes || snap.VRAMByNode[0].UsedBytes != 20<<30 {
		t.Fatalf("VRAMByNode = %+v", snap.VRAMByNode)
	}
}

// TestBuildClusterVRAMNodeUsedAtMSIsThatNodesOwnSampleTime proves
// vram_by_node[].used_as_of is the reporting node's own newest VRAM sample
// time, not the snapshot-wide AsOf (a different, healthier node's fresher
// reading must not make a stale node look current).
func TestBuildClusterVRAMNodeUsedAtMSIsThatNodesOwnSampleTime(t *testing.T) {
	store := newTestStore(t)
	inv := Inventory{
		Nodes: []NodeRef{{UUID: "stale-node", Name: "stale"}, {UUID: "fresh-node", Name: "fresh"}},
		GPUs: []GPURef{
			{NodeUUID: "stale-node", Index: 0, MemoryTotalBytes: 80 << 30},
			{NodeUUID: "fresh-node", Index: 0, MemoryTotalBytes: 80 << 30},
		},
	}
	deps := Deps{TSDB: store, Inventory: inventoryFunc(inv), Now: func() time.Time { return baseTime }}
	staleAt := baseTime.Add(-90 * time.Second)
	freshAt := baseTime
	writeSample(t, store, "gpuworks_gpu_memory_used_bytes", map[string]string{"node": "stale-node", "gpu": "0"}, float64(10<<30), staleAt)
	writeSample(t, store, "gpuworks_gpu_memory_used_bytes", map[string]string{"node": "fresh-node", "gpu": "0"}, float64(20<<30), freshAt)

	snap, err := BuildCluster(context.Background(), deps, time.Hour)
	if err != nil {
		t.Fatalf("BuildCluster: %v", err)
	}
	byUUID := map[string]VRAMNode{}
	for _, n := range snap.VRAMByNode {
		byUUID[n.NodeUUID] = n
	}
	stale, fresh := byUUID["stale-node"], byUUID["fresh-node"]
	if !stale.HasUsedBytes || stale.UsedAtMS != staleAt.UnixMilli() {
		t.Fatalf("stale node UsedAtMS = %d, want %d (its own sample time, not the fleet's freshest)", stale.UsedAtMS, staleAt.UnixMilli())
	}
	if !fresh.HasUsedBytes || fresh.UsedAtMS != freshAt.UnixMilli() {
		t.Fatalf("fresh node UsedAtMS = %d, want %d", fresh.UsedAtMS, freshAt.UnixMilli())
	}
}

func TestBuildClusterStaleWhenNoRecentData(t *testing.T) {
	store := newTestStore(t)
	deps := Deps{TSDB: store, Inventory: inventoryFunc(Inventory{}), Now: func() time.Time { return baseTime }}
	snap, err := BuildCluster(context.Background(), deps, time.Hour)
	if err != nil {
		t.Fatalf("BuildCluster: %v", err)
	}
	if !snap.Stale {
		t.Error("expected Stale=true with no data at all")
	}
	if !snap.AsOf.IsZero() {
		t.Errorf("AsOf = %v, want zero with no data at all", snap.AsOf)
	}
}

func TestBuildClusterFreshWithRecentTokenData(t *testing.T) {
	store := newTestStore(t)
	inv := Inventory{
		Deployments: []DeploymentRef{{ID: 1, Name: "svc-a"}},
		Instances:   []InstanceRef{{UUID: "a1", DeploymentID: 1}},
	}
	deps := Deps{TSDB: store, Inventory: inventoryFunc(inv), Now: func() time.Time { return baseTime }}
	labels := map[string]string{"instance": "a1", "deployment": "1"}
	// A chart range of 10 minutes keeps ChartStep at the collector's own
	// 10s tick (ChartStep floors to WindowStep for any rng <= 1200s), so
	// samples spaced exactly one tick apart land in distinct buckets - a
	// coarser bucket for a longer range could otherwise merge both samples
	// into one bucket and leave Rate() with nothing to diff.
	rng := 10 * time.Minute
	writeSample(t, store, "gpuworks_infer_generation_tokens_total", labels, 0, baseTime.Add(-10*time.Second))
	writeSample(t, store, "gpuworks_infer_generation_tokens_total", labels, 100, baseTime)

	snap, err := BuildCluster(context.Background(), deps, rng)
	if err != nil {
		t.Fatalf("BuildCluster: %v", err)
	}
	if snap.Stale {
		t.Errorf("expected Stale=false with a sample %v old", baseTime.Sub(snap.AsOf))
	}
	if len(snap.ClusterTokenRate) == 0 {
		t.Error("expected a non-empty ClusterTokenRate series")
	}
}

// TestBuildClusterAsOfFromNonTokenSamples is F2's regression case: a
// deployment with running samples but no token counters must still report a
// real AsOf/Stale, not the fallback that only looked at the cluster
// token-rate series.
func TestBuildClusterAsOfFromNonTokenSamples(t *testing.T) {
	store := newTestStore(t)
	inv := Inventory{
		Deployments: []DeploymentRef{{ID: 1, Name: "svc-a"}},
		Instances:   []InstanceRef{{UUID: "i1", DeploymentID: 1}},
	}
	deps := Deps{TSDB: store, Inventory: inventoryFunc(inv), Now: func() time.Time { return baseTime }}
	sampleAt := baseTime.Add(-5 * time.Second)
	writeSample(t, store, "gpuworks_infer_requests_running", map[string]string{"instance": "i1", "deployment": "1"}, 3, sampleAt)

	snap, err := BuildCluster(context.Background(), deps, time.Hour)
	if err != nil {
		t.Fatalf("BuildCluster: %v", err)
	}
	if snap.Stale {
		t.Errorf("expected Stale=false, AsOf=%v now=%v", snap.AsOf, baseTime)
	}
	// ComputeWindow's series come from QueryRange, which reports each point's
	// bucket boundary (floor(ts/step)*step), not the raw sample timestamp -
	// so AsOf lands on that boundary, up to one WindowStep earlier than
	// sampleAt, rather than matching it exactly.
	wantAsOf := time.UnixMilli((sampleAt.UnixMilli() / WindowStep.Milliseconds()) * WindowStep.Milliseconds()).UTC()
	if !snap.AsOf.Equal(wantAsOf) {
		t.Errorf("AsOf = %v, want %v (the running sample's bucket)", snap.AsOf, wantAsOf)
	}
}

// TestBuildClusterAsOfFromGPUSamplesWithEmptyFleet is F2's other regression
// case: zero deployments means ComputeWindow never runs, so AsOf must still
// come from vramByNode's GPU-memory-used samples.
func TestBuildClusterAsOfFromGPUSamplesWithEmptyFleet(t *testing.T) {
	store := newTestStore(t)
	inv := Inventory{
		Nodes: []NodeRef{{UUID: "n1", Name: "node-one"}},
		GPUs:  []GPURef{{NodeUUID: "n1", Index: 0, MemoryTotalBytes: 80 << 30}},
	}
	deps := Deps{TSDB: store, Inventory: inventoryFunc(inv), Now: func() time.Time { return baseTime }}
	sampleAt := baseTime.Add(-10 * time.Second)
	writeSample(t, store, "gpuworks_gpu_memory_used_bytes", map[string]string{"node": "n1", "gpu": "0"}, float64(10<<30), sampleAt)

	snap, err := BuildCluster(context.Background(), deps, time.Hour)
	if err != nil {
		t.Fatalf("BuildCluster: %v", err)
	}
	if snap.Stale {
		t.Error("expected Stale=false from GPU samples alone with zero deployments")
	}
	if !snap.AsOf.Equal(sampleAt) {
		t.Errorf("AsOf = %v, want %v (the GPU memory sample's time)", snap.AsOf, sampleAt)
	}
}

// TestBuildClusterWaitingP95SumsAcrossServices covers decision (d): the
// cluster-wide waiting p95 is the true combined queue depth (every service's
// waiting count summed per bucket, then the p95 of that), not a max or
// average of each service's own p95.
func TestBuildClusterWaitingP95SumsAcrossServices(t *testing.T) {
	store := newTestStore(t)
	inv := Inventory{
		Deployments: []DeploymentRef{{ID: 1, Name: "a"}, {ID: 2, Name: "b"}, {ID: 3, Name: "c"}},
		Instances: []InstanceRef{
			{UUID: "i1", DeploymentID: 1},
			{UUID: "i2", DeploymentID: 2},
			{UUID: "i3", DeploymentID: 3},
		},
	}
	deps := Deps{TSDB: store, Inventory: inventoryFunc(inv), Now: func() time.Time { return baseTime }}
	for _, id := range []string{"i1", "i2", "i3"} {
		writeSample(t, store, "gpuworks_infer_requests_waiting", map[string]string{"instance": id}, 10, baseTime)
	}

	snap, err := BuildCluster(context.Background(), deps, time.Hour)
	if err != nil {
		t.Fatalf("BuildCluster: %v", err)
	}
	if !snap.Signals.HasWaiting || snap.Signals.WaitingP95 != 30 {
		t.Fatalf("WaitingP95 = %v has=%v, want 30/true (10+10+10 summed per bucket)", snap.Signals.WaitingP95, snap.Signals.HasWaiting)
	}
	if len(snap.Signals.WaitingSeries) == 0 || snap.Signals.WaitingSeries[len(snap.Signals.WaitingSeries)-1].Value != 30 {
		t.Fatalf("WaitingSeries = %+v, want a non-empty series ending at 30 (the waiting signal's sparkline source)", snap.Signals.WaitingSeries)
	}
}

// TestBuildClusterServiceRowSortOrder proves service rows come back ordered
// by name, whatever order the inventory lists the deployments in (the
// monitor package no longer judges health, so there is no severity order).
func TestBuildClusterServiceRowSortOrder(t *testing.T) {
	store := newTestStore(t)
	inv := Inventory{
		Deployments: []DeploymentRef{
			{ID: 1, Name: "z-svc"},
			{ID: 2, Name: "a-svc"},
			{ID: 3, Name: "m-svc"},
		},
		Instances: []InstanceRef{
			{UUID: "i1", DeploymentID: 1, State: proto.InstRunning},
			{UUID: "i2", DeploymentID: 2, State: proto.InstRunning},
		},
	}
	deps := Deps{TSDB: store, Inventory: inventoryFunc(inv), Now: func() time.Time { return baseTime }}
	// A busy z-svc must not float above the idle services.
	writeSample(t, store, "gpuworks_infer_requests_running", map[string]string{"instance": "i1", "deployment": "1"}, 40, baseTime)

	snap, err := BuildCluster(context.Background(), deps, time.Hour)
	if err != nil {
		t.Fatalf("BuildCluster: %v", err)
	}
	if len(snap.Services) != 3 {
		t.Fatalf("Services = %d, want 3", len(snap.Services))
	}
	names := []string{snap.Services[0].Name, snap.Services[1].Name, snap.Services[2].Name}
	if names[0] != "a-svc" || names[1] != "m-svc" || names[2] != "z-svc" {
		t.Fatalf("sort order = %v, want [a-svc m-svc z-svc]", names)
	}
}

// TestBuildClusterRequiresTSDB covers decision (a): a nil Deps.TSDB is an
// error, with no silent fallback.
func TestBuildClusterRequiresTSDB(t *testing.T) {
	deps := Deps{Inventory: inventoryFunc(Inventory{}), Now: func() time.Time { return baseTime }}
	_, err := BuildCluster(context.Background(), deps, time.Hour)
	if err == nil {
		t.Fatal("expected an error for a nil Deps.TSDB")
	}
}

// TestBuildClusterPropagatesTSDBError is F12's coverage: a TSDB error on one
// metric must surface as an error, not silently produce a zeroed snapshot.
func TestBuildClusterPropagatesTSDBError(t *testing.T) {
	store := newTestStore(t)
	failing := &erroringStore{Store: store, failMetric: "gpuworks_infer_requests_running", err: errors.New("boom")}
	inv := Inventory{
		Deployments: []DeploymentRef{{ID: 1, Name: "svc"}},
		Instances:   []InstanceRef{{UUID: "i1", DeploymentID: 1}},
	}
	deps := Deps{TSDB: failing, Inventory: inventoryFunc(inv), Now: func() time.Time { return baseTime }}
	_, err := BuildCluster(context.Background(), deps, time.Hour)
	if err == nil {
		t.Fatal("expected BuildCluster to propagate the TSDB error, not return zeros")
	}
}
