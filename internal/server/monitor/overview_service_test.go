package monitor

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// TestBuildServicePropagatesTSDBError is F12's service-level coverage: a
// TSDB error on one metric must surface as an error, not silently produce a
// zeroed snapshot.
func TestBuildServicePropagatesTSDBError(t *testing.T) {
	store := newTestStore(t)
	failing := &erroringStore{Store: store, failMetric: "gpuworks_infer_requests_running", err: errors.New("boom")}
	inv := Inventory{
		Deployments: []DeploymentRef{{ID: 1, Name: "svc"}},
		Instances:   []InstanceRef{{UUID: "i1", DeploymentID: 1}},
	}
	deps := Deps{TSDB: failing, Inventory: inventoryFunc(inv), Now: func() time.Time { return baseTime }}
	_, err := BuildService(context.Background(), deps, 1, time.Hour)
	if err == nil {
		t.Fatal("expected BuildService to propagate the TSDB error, not return zeros")
	}
}

func TestBuildServiceDeploymentNotFound(t *testing.T) {
	deps := Deps{TSDB: newTestStore(t), Inventory: inventoryFunc(Inventory{}), Now: func() time.Time { return baseTime }}
	_, err := BuildService(context.Background(), deps, 42, time.Hour)
	var notFound *deploymentNotFoundError
	if !errors.As(err, &notFound) {
		t.Fatalf("err = %v, want a deploymentNotFoundError", err)
	}
}

func TestBuildServiceWaitingAbsentWithoutData(t *testing.T) {
	store := newTestStore(t)
	inv := Inventory{Deployments: []DeploymentRef{{ID: 1, Name: "svc"}}}
	deps := Deps{TSDB: store, Inventory: inventoryFunc(inv), Now: func() time.Time { return baseTime }}

	snap, err := BuildService(context.Background(), deps, 1, time.Hour)
	if err != nil {
		t.Fatalf("BuildService: %v", err)
	}
	if snap.Window.HasWaiting {
		t.Fatalf("Window.HasWaiting = true (avg %v), want false with no waiting data collected", snap.Window.WaitingAvg)
	}
	if len(snap.Charts["latency"]) != 0 {
		t.Fatalf("Charts[latency] = %v, want empty (no histograms yet)", snap.Charts["latency"])
	}
}

func TestBuildServiceWindowCarriesWaitingData(t *testing.T) {
	store := newTestStore(t)
	inv := Inventory{
		Deployments: []DeploymentRef{{ID: 1, Name: "svc"}},
		Instances:   []InstanceRef{{UUID: "i1", DeploymentID: 1}},
	}
	deps := Deps{TSDB: store, Inventory: inventoryFunc(inv), Now: func() time.Time { return baseTime }}
	labels := map[string]string{"instance": "i1", "deployment": "1"}
	// A real scrape tick reports requests_running alongside requests_waiting
	// (same /metrics call, same labels) - included here so coverage (derived
	// from requests_running) correctly finds i1 already deployment-labeled
	// instead of treating it as uncovered.
	writeSample(t, store, "gpuworks_infer_requests_running", labels, 0, baseTime)
	writeSample(t, store, "gpuworks_infer_requests_waiting", labels, 3, baseTime)

	snap, err := BuildService(context.Background(), deps, 1, time.Hour)
	if err != nil {
		t.Fatalf("BuildService: %v", err)
	}
	if !snap.Window.HasWaiting || snap.Window.WaitingAvg != 3 {
		t.Fatalf("Window.HasWaiting/WaitingAvg = %v/%v, want true/3", snap.Window.HasWaiting, snap.Window.WaitingAvg)
	}
}

// TestBuildServiceWaitingWithoutRequestsRunning is the hardening
// regression case: a Callable/custom engine spec may map requests_waiting
// without ever emitting gpuworks_infer_requests_running at all.
// Coverage-from-running then sees no data and treats i1 as "uncovered", but
// queryDeploymentMetric's own per-metric check must still find i1 already
// present in requests_waiting's own deployment-matched result and skip the
// fallback query, so the waiting value is not double counted.
func TestBuildServiceWaitingWithoutRequestsRunning(t *testing.T) {
	store := newTestStore(t)
	inv := Inventory{
		Deployments: []DeploymentRef{{ID: 2, Name: "svc"}},
		Instances:   []InstanceRef{{UUID: "i1", DeploymentID: 2}},
	}
	deps := Deps{TSDB: store, Inventory: inventoryFunc(inv), Now: func() time.Time { return baseTime }}
	labels := map[string]string{"instance": "i1", "deployment": "2"}
	// Deliberately no gpuworks_infer_requests_running sample at all.
	writeSample(t, store, "gpuworks_infer_requests_waiting", labels, 3, baseTime)

	snap, err := BuildService(context.Background(), deps, 2, time.Hour)
	if err != nil {
		t.Fatalf("BuildService: %v", err)
	}
	if !snap.Window.HasWaiting || snap.Window.WaitingAvg != 3 {
		t.Fatalf("Window.HasWaiting/WaitingAvg = %v/%v, want true/3 (not doubled)", snap.Window.HasWaiting, snap.Window.WaitingAvg)
	}
}

func TestBuildServiceReplicaRowsAndGPUs(t *testing.T) {
	store := newTestStore(t)
	inv := Inventory{
		Deployments: []DeploymentRef{{ID: 1, Name: "svc"}},
		Instances: []InstanceRef{
			{UUID: "i1", DeploymentID: 1, NodeUUID: "n1", State: "running", GPUIndexes: []int{0}},
		},
		Nodes: []NodeRef{{UUID: "n1", Name: "node-one"}},
	}
	deps := Deps{TSDB: store, Inventory: inventoryFunc(inv), Now: func() time.Time { return baseTime }}
	writeSample(t, store, "gpuworks_infer_requests_running", map[string]string{"instance": "i1"}, 2, baseTime)
	writeSample(t, store, "gpuworks_gpu_utilization", map[string]string{"node": "n1", "gpu": "0"}, 55, baseTime)
	writeSample(t, store, "gpuworks_gpu_temperature_celsius", map[string]string{"node": "n1", "gpu": "0"}, 70, baseTime)

	snap, err := BuildService(context.Background(), deps, 1, time.Hour)
	if err != nil {
		t.Fatalf("BuildService: %v", err)
	}
	if len(snap.Replicas) != 1 {
		t.Fatalf("Replicas = %d, want 1", len(snap.Replicas))
	}
	row := snap.Replicas[0]
	if row.NodeName != "node-one" {
		t.Errorf("NodeName = %q, want node-one", row.NodeName)
	}
	if !row.HasRunning || row.Running != 2 {
		t.Errorf("Running = %v (has=%v), want 2/true", row.Running, row.HasRunning)
	}
	if len(row.GPUs) != 1 || !row.GPUs[0].HasUtil || row.GPUs[0].UtilPct != 55 {
		t.Fatalf("GPUs = %+v, want one GPU with util=55", row.GPUs)
	}
	if !row.GPUs[0].HasTemp || row.GPUs[0].TempC != 70 {
		t.Fatalf("GPU temp = %+v, want 70", row.GPUs[0])
	}
}

// TestBuildServiceExcludesHeadlessInstancesFromReplicaTable proves a
// headless distributed worker never gets its own row in the replica table
// (plan §3.2 describes API-facing replicas): it is a process-group
// participant with no endpoint of its own, not a replica, even though it is
// still part of Inventory.Instances for this deployment.
func TestBuildServiceExcludesHeadlessInstancesFromReplicaTable(t *testing.T) {
	store := newTestStore(t)
	inv := Inventory{
		Deployments: []DeploymentRef{{ID: 1, Name: "svc"}},
		Instances: []InstanceRef{
			{UUID: "leader", DeploymentID: 1, NodeUUID: "n1", State: proto.InstRunning},
			{UUID: "follower", DeploymentID: 1, NodeUUID: "n1", State: proto.InstRunning, Headless: true},
		},
		Nodes: []NodeRef{{UUID: "n1", Name: "node-one"}},
	}
	deps := Deps{TSDB: store, Inventory: inventoryFunc(inv), Now: func() time.Time { return baseTime }}

	snap, err := BuildService(context.Background(), deps, 1, time.Hour)
	if err != nil {
		t.Fatalf("BuildService: %v", err)
	}
	if len(snap.Replicas) != 1 || snap.Replicas[0].InstanceUUID != "leader" {
		t.Fatalf("Replicas = %+v, want exactly one row for the leader", snap.Replicas)
	}
}

func TestBuildServiceChartsIncludeConcurrencyAndThroughput(t *testing.T) {
	store := newTestStore(t)
	inv := Inventory{
		Deployments: []DeploymentRef{{ID: 1, Name: "svc"}},
		Instances:   []InstanceRef{{UUID: "i1", DeploymentID: 1}},
	}
	deps := Deps{TSDB: store, Inventory: inventoryFunc(inv), Now: func() time.Time { return baseTime }}
	labels := map[string]string{"instance": "i1", "deployment": "1"}
	writeSample(t, store, "gpuworks_infer_requests_running", labels, 4, baseTime)
	writeSample(t, store, "gpuworks_infer_requests_waiting", labels, 1, baseTime)

	snap, err := BuildService(context.Background(), deps, 1, 10*time.Minute)
	if err != nil {
		t.Fatalf("BuildService: %v", err)
	}
	if len(snap.Charts["concurrency"]) == 0 {
		t.Fatal("Charts[concurrency] is empty")
	}
	foundRunning, foundWaiting := false, false
	for _, s := range snap.Charts["concurrency"] {
		if s.Name == "running" {
			foundRunning = true
		}
		if s.Name == "waiting" {
			foundWaiting = true
		}
	}
	if !foundRunning || !foundWaiting {
		t.Fatalf("Charts[concurrency] = %+v, want both running and waiting series", snap.Charts["concurrency"])
	}
}

// TestBuildServiceConcurrencyChartIsPerInstance proves the concurrency chart
// draws one running/waiting line per replica (plan §6: "labels.metric =
// running|waiting, labels.instance"), not one line combining every
// replica's load into a single deployment total - that would hide exactly
// the "one replica is overloaded while its siblings idle" case the
// per-replica breakdown exists to show.
func TestBuildServiceConcurrencyChartIsPerInstance(t *testing.T) {
	store := newTestStore(t)
	inv := Inventory{
		Deployments: []DeploymentRef{{ID: 1, Name: "svc"}},
		Instances: []InstanceRef{
			{UUID: "i1", DeploymentID: 1},
			{UUID: "i2", DeploymentID: 1},
		},
	}
	deps := Deps{TSDB: store, Inventory: inventoryFunc(inv), Now: func() time.Time { return baseTime }}
	writeSample(t, store, "gpuworks_infer_requests_running", map[string]string{"instance": "i1", "deployment": "1"}, 10, baseTime)
	writeSample(t, store, "gpuworks_infer_requests_running", map[string]string{"instance": "i2", "deployment": "1"}, 30, baseTime)
	writeSample(t, store, "gpuworks_infer_requests_waiting", map[string]string{"instance": "i1", "deployment": "1"}, 1, baseTime)
	writeSample(t, store, "gpuworks_infer_requests_waiting", map[string]string{"instance": "i2", "deployment": "1"}, 9, baseTime)

	snap, err := BuildService(context.Background(), deps, 1, 10*time.Minute)
	if err != nil {
		t.Fatalf("BuildService: %v", err)
	}
	running := map[string]float64{}
	waiting := map[string]float64{}
	for _, s := range snap.Charts["concurrency"] {
		if len(s.Points) == 0 {
			t.Fatalf("series %+v has no points", s)
		}
		v := s.Points[len(s.Points)-1].Value
		switch s.Name {
		case "running":
			running[s.Labels["instance"]] = v
		case "waiting":
			waiting[s.Labels["instance"]] = v
		default:
			t.Fatalf("unexpected concurrency series name %q", s.Name)
		}
	}
	if running["i1"] != 10 || running["i2"] != 30 {
		t.Fatalf("running by instance = %+v, want i1=10 i2=30 (not combined into one 40)", running)
	}
	if waiting["i1"] != 1 || waiting["i2"] != 9 {
		t.Fatalf("waiting by instance = %+v, want i1=1 i2=9", waiting)
	}
}

// TestBuildServiceKVAndThroughputChartsArePerInstance matches
// ui/tests/fixtures/monitor-deployment.json (which labels every kv/throughput
// series with "instance" alongside "metric", not just concurrency): a
// replica with a saturated KV cache and high throughput must not have that
// signal averaged/summed away with its siblings.
func TestBuildServiceKVAndThroughputChartsArePerInstance(t *testing.T) {
	store := newTestStore(t)
	inv := Inventory{
		Deployments: []DeploymentRef{{ID: 1, Name: "svc"}},
		Instances: []InstanceRef{
			{UUID: "i1", DeploymentID: 1},
			{UUID: "i2", DeploymentID: 1},
		},
	}
	deps := Deps{TSDB: store, Inventory: inventoryFunc(inv), Now: func() time.Time { return baseTime }}
	writeSample(t, store, "gpuworks_infer_gpu_cache_usage_percent", map[string]string{"instance": "i1", "deployment": "1"}, 0.94, baseTime)
	writeSample(t, store, "gpuworks_infer_gpu_cache_usage_percent", map[string]string{"instance": "i2", "deployment": "1"}, 0.40, baseTime)
	writeSample(t, store, "gpuworks_infer_prompt_tokens_total", map[string]string{"instance": "i1", "deployment": "1"}, 100, baseTime.Add(-time.Second))
	writeSample(t, store, "gpuworks_infer_prompt_tokens_total", map[string]string{"instance": "i1", "deployment": "1"}, 200, baseTime)
	writeSample(t, store, "gpuworks_infer_prompt_tokens_total", map[string]string{"instance": "i2", "deployment": "1"}, 10, baseTime.Add(-time.Second))
	writeSample(t, store, "gpuworks_infer_prompt_tokens_total", map[string]string{"instance": "i2", "deployment": "1"}, 20, baseTime)

	snap, err := BuildService(context.Background(), deps, 1, 10*time.Minute)
	if err != nil {
		t.Fatalf("BuildService: %v", err)
	}
	kvByInstance := map[string]float64{}
	for _, s := range snap.Charts["kv"] {
		if s.Name != "usage" {
			continue
		}
		kvByInstance[s.Labels["instance"]] = s.Points[len(s.Points)-1].Value
	}
	if kvByInstance["i1"] != 0.94 || kvByInstance["i2"] != 0.40 {
		t.Fatalf("kv usage by instance = %+v, want i1=0.94 i2=0.40 (not blended)", kvByInstance)
	}
	throughputByInstance := map[string]bool{}
	for _, s := range snap.Charts["throughput"] {
		if s.Name == "prompt_tokens_per_sec" {
			throughputByInstance[s.Labels["instance"]] = true
		}
	}
	if !throughputByInstance["i1"] || !throughputByInstance["i2"] {
		t.Fatalf("throughput series must be labeled per instance: %+v", snap.Charts["throughput"])
	}
}

// TestBuildServiceKVHitRateChartPerInstance covers the kv chart's hit-rate
// line for both engine shapes: a vLLM replica only exports prefix-cache
// counters (hits/queries per step, a step without queries is a gap), an
// SGLang replica only its instant gauge. Counters win when both exist, as in
// the window's single number.
func TestBuildServiceKVHitRateChartPerInstance(t *testing.T) {
	store := newTestStore(t)
	inv := Inventory{
		Deployments: []DeploymentRef{{ID: 1, Name: "svc"}},
		Instances: []InstanceRef{
			{UUID: "vllm", DeploymentID: 1},
			{UUID: "sglang", DeploymentID: 1},
		},
	}
	deps := Deps{TSDB: store, Inventory: inventoryFunc(inv), Now: func() time.Time { return baseTime }}
	vllm := map[string]string{"instance": "vllm", "deployment": "1"}
	queries := []float64{100, 200, 200, 300}
	hits := []float64{50, 130, 130, 155}
	for i := range queries {
		at := baseTime.Add(time.Duration(i-3) * WindowStep)
		writeSample(t, store, "gpuworks_infer_kv_queries_total", vllm, queries[i], at)
		writeSample(t, store, "gpuworks_infer_kv_hits_total", vllm, hits[i], at)
	}
	writeSample(t, store, "gpuworks_infer_kv_hit_rate_instant", vllm, 0.1, baseTime)
	writeSample(t, store, "gpuworks_infer_kv_hit_rate_instant", map[string]string{"instance": "sglang", "deployment": "1"}, 0.42, baseTime)

	snap, err := BuildService(context.Background(), deps, 1, 10*time.Minute)
	if err != nil {
		t.Fatalf("BuildService: %v", err)
	}
	got := map[string][]float64{}
	for _, s := range snap.Charts["kv"] {
		if s.Name != "hit_rate" {
			continue
		}
		for _, p := range s.Points {
			got[s.Labels["instance"]] = append(got[s.Labels["instance"]], p.Value)
		}
	}
	want := map[string][]float64{"vllm": {0.8, 0.25}, "sglang": {0.42}}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("kv hit_rate by instance = %+v, want %+v", got, want)
	}
}

func TestBuildServiceRequiresTSDB(t *testing.T) {
	inv := Inventory{Deployments: []DeploymentRef{{ID: 1, Name: "svc"}}}
	deps := Deps{Inventory: inventoryFunc(inv), Now: func() time.Time { return baseTime }}
	_, err := BuildService(context.Background(), deps, 1, time.Hour)
	if err == nil {
		t.Fatal("expected an error for a nil Deps.TSDB")
	}
}

// TestBuildServiceWindowCarriesEntryPrefillDecodeSignals proves the Window
// BuildService returns carries the request rate, prefill time and TPOT
// signals once the underlying phase2 series exist.
func TestBuildServiceWindowCarriesEntryPrefillDecodeSignals(t *testing.T) {
	store := newTestStore(t)
	inv := Inventory{
		Deployments: []DeploymentRef{{ID: 90, Name: "svc"}},
		Instances:   []InstanceRef{{UUID: "i1", DeploymentID: 90}},
	}
	deps := Deps{TSDB: store, Inventory: inventoryFunc(inv), Now: func() time.Time { return baseTime }}

	classLabels := map[string]string{"deployment": "90", "class": "2xx"}
	writeSample(t, store, "gpuworks_proxy_requests_total", classLabels, 0, baseTime.Add(-10*time.Second))
	writeSample(t, store, "gpuworks_proxy_requests_total", classLabels, 100, baseTime)
	labels := map[string]string{"instance": "i1", "deployment": "90"}
	writeSample(t, store, "gpuworks_infer_prefill_time_seconds", withQuantileLabel(labels, "0.95"), 1.0, baseTime)
	writeSample(t, store, "gpuworks_infer_tpot_seconds", withQuantileLabel(labels, "0.95"), 0.05, baseTime)

	snap, err := BuildService(context.Background(), deps, 90, time.Hour)
	if err != nil {
		t.Fatalf("BuildService: %v", err)
	}
	w := snap.Window
	if !w.HasRequestRate || w.RequestsPerSec <= 0 {
		t.Errorf("Window request rate = %v (has=%v), want collected and positive", w.RequestsPerSec, w.HasRequestRate)
	}
	if !w.HasPrefillTime || w.PrefillTimeP95Sec != 1.0 {
		t.Errorf("Window prefill p95 = %v (has=%v), want 1.0/true", w.PrefillTimeP95Sec, w.HasPrefillTime)
	}
	if !w.HasTPOT || w.TPOTP95Sec != 0.05 {
		t.Errorf("Window TPOT p95 = %v (has=%v), want 0.05/true", w.TPOTP95Sec, w.HasTPOT)
	}
}

// withQuantileLabel copies base and adds a "quantile" key, for tests that
// write ingest-time histogram quantile gauges.
func withQuantileLabel(base map[string]string, quantile string) map[string]string {
	out := map[string]string{"quantile": quantile}
	for k, v := range base {
		out[k] = v
	}
	return out
}

// TestBuildServiceLatencyAndStageTimeCharts proves the latency/stage_time/
// preemptions charts (empty in phase 1) now carry data once the underlying
// histograms exist.
func TestBuildServiceLatencyAndStageTimeCharts(t *testing.T) {
	store := newTestStore(t)
	inv := Inventory{
		Deployments: []DeploymentRef{{ID: 91, Name: "svc"}},
		Instances:   []InstanceRef{{UUID: "i1", DeploymentID: 91}},
	}
	deps := Deps{TSDB: store, Inventory: inventoryFunc(inv), Now: func() time.Time { return baseTime }}
	labels := map[string]string{"instance": "i1", "deployment": "91"}
	writeSample(t, store, "gpuworks_infer_ttft_seconds", withQuantileLabel(labels, "0.95"), 2.0, baseTime)
	writeSample(t, store, "gpuworks_infer_queue_time_seconds", withQuantileLabel(labels, "0.95"), 0.3, baseTime)
	writeSample(t, store, "gpuworks_infer_preemptions_total", labels, 5, baseTime.Add(-10*time.Second))
	writeSample(t, store, "gpuworks_infer_preemptions_total", labels, 6, baseTime)

	snap, err := BuildService(context.Background(), deps, 91, time.Hour)
	if err != nil {
		t.Fatalf("BuildService: %v", err)
	}
	if len(snap.Charts["latency"]) == 0 {
		t.Error("Charts[latency] = empty, want ttft_p95 data")
	}
	if len(snap.Charts["stage_time"]) == 0 {
		t.Error("Charts[stage_time] = empty, want queue_ms data")
	}
	if len(snap.Charts["preemptions"]) == 0 {
		t.Error("Charts[preemptions] = empty, want preemptions_per_s data")
	}
}

// TestBuildServiceReplicaGPUMemBW proves the replica table's per-GPU rows
// now also carry MemBW (in addition to util/temp).
func TestBuildServiceReplicaGPUMemBW(t *testing.T) {
	store := newTestStore(t)
	inv := Inventory{
		Deployments: []DeploymentRef{{ID: 92, Name: "svc"}},
		Instances:   []InstanceRef{{UUID: "i1", DeploymentID: 92, NodeUUID: "n1", GPUIndexes: []int{0}}},
	}
	deps := Deps{TSDB: store, Inventory: inventoryFunc(inv), Now: func() time.Time { return baseTime }}
	writeSample(t, store, "gpuworks_gpu_memory_bandwidth_utilization", map[string]string{"node": "n1", "gpu": "0"}, 77, baseTime)

	snap, err := BuildService(context.Background(), deps, 92, time.Hour)
	if err != nil {
		t.Fatalf("BuildService: %v", err)
	}
	if len(snap.Replicas) != 1 || len(snap.Replicas[0].GPUs) != 1 {
		t.Fatalf("Replicas = %+v", snap.Replicas)
	}
	gpu := snap.Replicas[0].GPUs[0]
	if !gpu.HasMemBW || gpu.MemBWPct != 77 {
		t.Fatalf("GPU MemBW = %v has=%v, want 77/true", gpu.MemBWPct, gpu.HasMemBW)
	}
}
