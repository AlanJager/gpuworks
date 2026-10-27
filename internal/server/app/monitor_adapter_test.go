package app

import (
	"context"
	"encoding/json"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/healthcheck"
	"gpuworks.ai/gpuworks/internal/server/monitorapi"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// This test wires the real monitor+monitorapi packages (not a fake) against
// a temp SQLite tsdb and an in-memory store, the way app.go actually
// assembles the Monitor service (newMonitorAPIService) - the fake in
// internal/server/api/monitor_test.go proves the HTTP contract, this proves
// the wiring between store, monitorapi and monitor actually
// produces a correct end-to-end overview and deployment response. The
// fixture builders here are shared with monitor_adapter_extra_test.go.

func newMonitorTestStore(t *testing.T) *store.Store {
	t.Helper()
	st, err := store.Open("file:monitor_adapter_test_"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatalf("open store: %v", err)
	}
	t.Cleanup(func() { _ = st.Close() })
	return st
}

func newMonitorTestTSDB(t *testing.T) tsdb.Store {
	t.Helper()
	ts, err := tsdb.OpenSQLite(filepath.Join(t.TempDir(), "metrics.db"))
	if err != nil {
		t.Fatalf("open tsdb: %v", err)
	}
	t.Cleanup(func() { _ = ts.Close() })
	return ts
}

func writeMonitorSample(t *testing.T, ts tsdb.Store, metric string, labels map[string]string, value float64, at time.Time) {
	t.Helper()
	if err := ts.WriteBatch([]tsdb.Sample{{Metric: metric, Labels: labels, Value: value, TimeMS: at.UnixMilli()}}); err != nil {
		t.Fatalf("write %s: %v", metric, err)
	}
}

// monitorFixture is one running, instrumented deployment: a node with one
// GPU, a deployment with a resolved launch argv, a running instance on that GPU, and a run of tsdb samples
// ending at "now" for every metric ComputeWindow reads.
type monitorFixture struct {
	st       *store.Store
	ts       tsdb.Store
	node     *store.Node
	dep      *store.Deployment
	inst     *store.Instance
	now      time.Time
	nodeUUID string
}

// newMonitorFixtureBase creates the node/deployment/instance every fixture
// in this file shares - a single running vLLM replica with a resolved
// launch argv - but writes no tsdb samples: callers below write different
// sample shapes.
func newMonitorFixtureBase(t *testing.T) monitorFixture {
	t.Helper()
	st := newMonitorTestStore(t)
	ts := newMonitorTestTSDB(t)

	node, err := st.RegisterNode("monitor-node", store.HashToken("monitor-token"), "monitor-node")
	if err != nil {
		t.Fatalf("register node: %v", err)
	}
	if err := st.UpdateNodeStatus(node.UUID, proto.NodeStatus{
		Sys: proto.SysInfo{Hostname: "gpu3", GPUVendor: "nvidia", GPUDriver: "550.90"},
		GPUs: []proto.GPUInfo{{
			Index: 0, UUID: "GPU-a", Name: "NVIDIA H100", Vendor: "nvidia", MemoryTotalBytes: 80 << 30, Healthy: true, HealthStatus: "OK",
		}},
	}); err != nil {
		t.Fatalf("update node status: %v", err)
	}
	node, err = st.NodeWithGPUs(node.UUID)
	if err != nil {
		t.Fatalf("reload node: %v", err)
	}

	dep, err := st.CreateDeployment(&store.Deployment{
		Name: "Qwen3-32B", Engine: "vllm", Replicas: 1, NodeID: node.ID, State: store.DeployActive,
	})
	if err != nil {
		t.Fatalf("create deployment: %v", err)
	}

	desired := proto.InstanceDesiredState{
		ResolvedLaunch: proto.ResolvedLaunch{Argv: []string{"--max-num-seqs", "256"}},
		UUID:           "monitor-instance",
		DeploymentID:   dep.ID,
		Engine:         "vllm",
		DesiredState:   "running",
		GPUIndexes:     []int{0},
	}
	specJSON, err := json.Marshal(desired)
	if err != nil {
		t.Fatal(err)
	}
	inst, err := st.CreateInstance(&store.Instance{
		UUID: desired.UUID, DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning,
		GPUIndexes: "[0]", DesiredStateJSON: string(specJSON),
	})
	if err != nil {
		t.Fatalf("create instance: %v", err)
	}

	now := time.Now().UTC().Truncate(time.Second)
	return monitorFixture{st: st, ts: ts, node: node, dep: dep, inst: inst, now: now, nodeUUID: node.UUID}
}

func buildMonitorFixture(t *testing.T) monitorFixture {
	t.Helper()
	fx := newMonitorFixtureBase(t)
	depLabels := map[string]string{"deployment": strconv.FormatUint(uint64(fx.dep.ID), 10)}
	// Spaced a full minute apart, comfortably wider than both the 10s
	// bucket ComputeWindow's diagnosis window uses and the ~30s bucket the
	// cluster-level chart range (an hour, in these tests) derives - so
	// every sample lands in its own bucket regardless of where the buckets
	// happen to fall, and Rate() always sees at least two points to diff.
	for i, offset := range []time.Duration{2 * time.Minute, time.Minute, 0} {
		at := fx.now.Add(-offset)
		writeMonitorSample(t, fx.ts, "gpuworks_infer_requests_running", depLabels, float64(30+i*4), at)
		writeMonitorSample(t, fx.ts, "gpuworks_infer_requests_waiting", depLabels, float64(5+i), at)
		writeMonitorSample(t, fx.ts, "gpuworks_infer_gpu_cache_usage_percent", depLabels, 0.7+float64(i)*0.05, at)
		writeMonitorSample(t, fx.ts, "gpuworks_infer_prompt_tokens_total", depLabels, float64(100+i*50), at)
		writeMonitorSample(t, fx.ts, "gpuworks_infer_generation_tokens_total", depLabels, float64(50+i*20), at)
	}
	writeMonitorSample(t, fx.ts, "gpuworks_gpu_memory_used_bytes", map[string]string{"node": fx.node.UUID}, float64(40<<30), fx.now)
	return fx
}

func TestMonitorServiceOverviewEndToEnd(t *testing.T) {
	fx := buildMonitorFixture(t)
	svc := newMonitorAPIService(fx.st, fx.ts, func() time.Time { return fx.now }, nil, nil, nil)

	out, err := svc.Overview(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if out.Stale {
		t.Fatalf("overview reported stale with fresh samples: %+v", out)
	}
	if out.AsOf == 0 {
		t.Fatal("as_of must be non-zero when samples exist")
	}

	if len(out.Signals) != 8 {
		t.Fatalf("signals = %+v, want the fixed 8-entry row", out.Signals)
	}
	ready := out.Signals[0]
	if ready.Key != "ready_replicas" || !ready.Collected || ready.Value == nil || *ready.Value != 1 || ready.Denominator == nil || *ready.Denominator != 1 {
		t.Fatalf("ready_replicas signal = %+v", ready)
	}

	if len(out.Services) != 1 {
		t.Fatalf("services = %+v, want exactly the one deployment", out.Services)
	}
	svcRow := out.Services[0]
	if svcRow.DeploymentID != fx.dep.ID || svcRow.Name != "Qwen3-32B" || svcRow.Engine != "vllm" {
		t.Fatalf("service row = %+v", svcRow)
	}
	if svcRow.State != "running" {
		t.Fatalf("service state = %q, want running", svcRow.State)
	}
	if svcRow.Placement != "TP1 · gpu3" {
		t.Fatalf("placement = %q, want %q", svcRow.Placement, "TP1 · gpu3")
	}
	if svcRow.Running == nil || svcRow.Waiting == nil || svcRow.KVUsagePct == nil || svcRow.PromptTokS == nil || svcRow.GenerationTokS == nil {
		t.Fatalf("service row must carry collected metrics: %+v", svcRow)
	}
	if svcRow.RequestRate != nil || svcRow.TTFTP95Ms != nil {
		t.Fatalf("request_rate/ttft_p95_ms must stay null this stage: %+v", svcRow)
	}
	if svcRow.Health.Status != "off" || svcRow.Health.Recent == nil || len(svcRow.Health.Recent) != 0 {
		t.Fatalf("service row health = %+v, want the health check off with an empty (non-nil) recent list", svcRow.Health)
	}

	if len(out.VRAMByNode) != 1 {
		t.Fatalf("vram_by_node = %+v", out.VRAMByNode)
	}
	vram := out.VRAMByNode[0]
	if vram.NodeUUID != fx.nodeUUID || vram.GPUCount != 1 || vram.TotalBytes != 80<<30 {
		t.Fatalf("vram node = %+v", vram)
	}
	if vram.UsedBytes == nil || *vram.UsedBytes != 40<<30 {
		t.Fatalf("vram used_bytes = %+v, want 40GiB", vram.UsedBytes)
	}
	if vram.UsedAsOf == nil || *vram.UsedAsOf != fx.now.UnixMilli() {
		t.Fatalf("vram used_as_of = %v, want the node's own sample time %d", vram.UsedAsOf, fx.now.UnixMilli())
	}

	if len(out.ClusterTokens.Prompt.Points) == 0 || len(out.ClusterTokens.Generation.Points) == 0 {
		t.Fatalf("cluster_tokens must carry both prompt and generation series: %+v", out.ClusterTokens)
	}
}

func TestMonitorServiceDeploymentEndToEnd(t *testing.T) {
	fx := buildMonitorFixture(t)
	svc := newMonitorAPIService(fx.st, fx.ts, func() time.Time { return fx.now }, nil, nil, nil)

	out, err := svc.Deployment(context.Background(), fx.dep.ID, time.Hour)
	if err != nil {
		t.Fatalf("Deployment: %v", err)
	}
	if out.Deployment.ID != fx.dep.ID || out.Deployment.Name != "Qwen3-32B" || out.Deployment.Engine != "vllm" {
		t.Fatalf("deployment summary = %+v", out.Deployment)
	}
	if out.Deployment.Replicas.Ready != 1 || out.Deployment.Replicas.Desired != 1 {
		t.Fatalf("replicas = %+v", out.Deployment.Replicas)
	}
	if out.Health.Status != "off" || out.Health.Recent == nil {
		t.Fatalf("deployment health = %+v, want the health check off with an empty (non-nil) recent list", out.Health)
	}
	if out.Alignment != nil || out.TTFTBreakdown != nil {
		t.Fatalf("alignment/ttft_breakdown must stay nil this stage: %+v / %+v", out.Alignment, out.TTFTBreakdown)
	}
	if len(out.Replicas) != 1 {
		t.Fatalf("replicas = %+v", out.Replicas)
	}
	rep := out.Replicas[0]
	if rep.InstanceUUID != fx.inst.UUID || rep.State != "running" || rep.NodeName != "gpu3" {
		t.Fatalf("replica row = %+v", rep)
	}
	if len(rep.GPUIndexes) != 1 || rep.GPUIndexes[0] != 0 {
		t.Fatalf("replica gpu_indexes = %+v", rep.GPUIndexes)
	}
	if len(rep.GPUs) != 1 || rep.GPUs[0].Index != 0 {
		t.Fatalf("replica gpus = %+v", rep.GPUs)
	}
	if len(out.Charts.Concurrency) == 0 {
		t.Fatalf("charts.concurrency must have running/waiting series: %+v", out.Charts)
	}
	if out.Charts.Latency == nil || len(out.Charts.Latency) != 0 {
		t.Fatalf("charts.latency must be an empty (non-nil) slice this stage: %#v", out.Charts.Latency)
	}
}

// TestMonitorServiceReportsTheDeploymentHealthCheck proves the app wiring of
// the health source: a deployment with the check on reads what the checker
// holds for its own id, on both the overview row and the deployment page,
// and one with it off still reads off.
func TestMonitorServiceReportsTheDeploymentHealthCheck(t *testing.T) {
	fx := buildMonitorFixture(t)
	var asked []uint
	health := func(id uint) healthcheck.Health {
		asked = append(asked, id)
		return healthcheck.Health{
			Status: healthcheck.StatusOK,
			Recent: []healthcheck.Probe{
				{AtMS: fx.now.UnixMilli() - 10_000, Status: healthcheck.StatusTimeout, LatencyMS: 15_000},
				{AtMS: fx.now.UnixMilli(), Status: healthcheck.StatusOK, LatencyMS: 120, HTTPStatus: 200, InstanceUUID: fx.inst.UUID},
			},
		}
	}
	newSvc := func() *monitorapi.Service {
		return newMonitorAPIService(fx.st, fx.ts, func() time.Time { return fx.now }, nil, health, nil)
	}

	off, err := newSvc().Overview(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if len(off.Services) != 1 || off.Services[0].Health.Status != "off" || len(asked) != 0 {
		t.Fatalf("health check off: row = %+v, health asked for %v; want off and the checker not consulted", off.Services, asked)
	}

	if err := fx.st.SetDeploymentHealthCheck(fx.dep.ID, true, 15); err != nil {
		t.Fatalf("SetDeploymentHealthCheck: %v", err)
	}
	on, err := newSvc().Overview(context.Background(), time.Hour)
	if err != nil {
		t.Fatalf("Overview: %v", err)
	}
	if len(on.Services) != 1 {
		t.Fatalf("services = %+v", on.Services)
	}
	h := on.Services[0].Health
	if h.Status != "ok" || h.ThresholdSec != 15 || len(h.Recent) != 2 || h.Last == nil || h.Last.LatencyMS != 120 || h.Last.HTTPStatus != 200 {
		t.Fatalf("overview health = %+v, want ok, 15s threshold, two probes, the latest one last", h)
	}
	if h.RecentMeanMS == nil || *h.RecentMeanMS != 120 || h.RecentOK != 1 {
		t.Fatalf("recent mean = %v over %d ok probes, want 120ms over 1 (the timeout is not averaged)", h.RecentMeanMS, h.RecentOK)
	}
	if len(asked) != 1 || asked[0] != fx.dep.ID {
		t.Fatalf("health asked for %v, want only deployment %d", asked, fx.dep.ID)
	}

	dep, err := newSvc().Deployment(context.Background(), fx.dep.ID, time.Hour)
	if err != nil {
		t.Fatalf("Deployment: %v", err)
	}
	if dep.Health.Status != "ok" || dep.Health.ThresholdSec != 15 || len(dep.Health.Recent) != 2 {
		t.Fatalf("deployment health = %+v, want the same ok reading as the overview row", dep.Health)
	}
}

func TestMonitorDisconnectedReplicaRetainsObservation(t *testing.T) {
	fx := newMonitorFixtureBase(t)
	svc := newMonitorAPIService(fx.st, fx.ts, func() time.Time { return fx.now }, nil, nil, nil).At(fx.now)
	for _, tc := range []struct {
		nodeState, state string
		ready            int
	}{
		{store.NodeStateOnline, "running", 1},
		{store.NodeStateOffline, "offline", 0},
		{store.NodeStateOnline, "running", 1},
	} {
		if err := fx.st.SetNodeState(fx.node.UUID, tc.nodeState); err != nil {
			t.Fatal(err)
		}
		got, err := svc.Deployment(context.Background(), fx.dep.ID, time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		overview, err := svc.Overview(context.Background(), time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		if len(overview.Services) != 1 || overview.Services[0].State != tc.state || got.Deployment.Replicas.Ready != tc.ready {
			t.Fatalf("deployment = %+v", got.Deployment)
		}
		inst, err := fx.st.InstanceByUUID(fx.inst.UUID)
		if err != nil || inst.State != proto.InstRunning {
			t.Fatalf("observation = %+v, %v", inst, err)
		}
	}
}
