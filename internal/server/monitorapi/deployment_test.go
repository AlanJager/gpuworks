package monitorapi

import (
	"context"
	"fmt"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/monitor"
	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// TestChartSeriesDTORenamesToContractNames proves monitor's internal chart
// series names ("usage", "hit_rate", "prompt_tokens_per_sec",
// "generation_tokens_per_sec") reach the wire as the contract's exact
// labels.metric values.
func TestChartSeriesDTORenamesToContractNames(t *testing.T) {
	for _, tc := range []struct{ in, want string }{
		{"usage", "usage_pct"},
		{"hit_rate", "hit_rate_pct"},
		{"prompt_tokens_per_sec", "prompt_tok_s"},
		{"generation_tokens_per_sec", "generation_tok_s"},
		{"running", "running"}, // concurrency names already match the contract
		{"waiting", "waiting"},
	} {
		series := []monitor.ChartSeries{{Name: tc.in, Points: []tsdb.Point{{TimeMS: 0, Value: 1}}}}
		out := chartSeriesDTO(series, 1)
		if len(out) != 1 || out[0].Labels["metric"] != tc.want {
			t.Fatalf("chartSeriesDTO(%q) metric label = %q, want %q", tc.in, out[0].Labels["metric"], tc.want)
		}
	}
}

// TestChartSeriesDTOScalesKVToPercent proves kv values, which are 0..1
// fractions internally (monitor.Window/ChartSeries), reach the wire as the
// contract's usage_pct/hit_rate_pct percentages.
func TestChartSeriesDTOScalesKVToPercent(t *testing.T) {
	series := []monitor.ChartSeries{{Name: "usage", Points: []tsdb.Point{{TimeMS: 0, Value: 0.94}}}}
	out := chartsDTO(map[string][]monitor.ChartSeries{"kv": series})
	if len(out.KV) != 1 || len(out.KV[0].Points) != 1 || out.KV[0].Points[0][1] != 94 {
		t.Fatalf("kv chart = %+v, want the 0.94 fraction scaled to 94", out.KV)
	}
}

// TestChartSeriesDTOThroughputIsNotScaled proves the scale factor is
// per-group, not global: throughput values (already tok/s) must pass
// through unchanged even though they go through the same conversion path as
// kv.
func TestChartSeriesDTOThroughputIsNotScaled(t *testing.T) {
	series := []monitor.ChartSeries{{Name: "prompt_tokens_per_sec", Points: []tsdb.Point{{TimeMS: 0, Value: 1500}}}}
	out := chartsDTO(map[string][]monitor.ChartSeries{"throughput": series})
	if len(out.Throughput) != 1 || out.Throughput[0].Points[0][1] != 1500 {
		t.Fatalf("throughput chart = %+v, want 1500 unchanged", out.Throughput)
	}
}

// TestChartSeriesDTOPropagatesInstanceLabel proves a monitor.ChartSeries
// already labeled {instance: uuid} (see monitor/overview_service.go's
// BuildService, which groups concurrency/kv/throughput per replica) keeps
// that label on the wire alongside "metric".
func TestChartSeriesDTOPropagatesInstanceLabel(t *testing.T) {
	series := []monitor.ChartSeries{
		{Name: "running", Labels: map[string]string{"instance": "i1"}, Points: []tsdb.Point{{TimeMS: 0, Value: 5}}},
		{Name: "running", Labels: map[string]string{"instance": "i2"}, Points: []tsdb.Point{{TimeMS: 0, Value: 9}}},
	}
	out := chartSeriesDTO(series, 1)
	if len(out) != 2 {
		t.Fatalf("chartSeriesDTO = %+v, want two per-instance series", out)
	}
	byInstance := map[string]float64{}
	for _, s := range out {
		if s.Labels["metric"] != "running" {
			t.Fatalf("series %+v missing metric=running", s)
		}
		byInstance[s.Labels["instance"]] = s.Points[0][1]
	}
	if byInstance["i1"] != 5 || byInstance["i2"] != 9 {
		t.Fatalf("byInstance = %+v, want i1=5 i2=9", byInstance)
	}
}

func TestChartsDTOEmptyGroupsAreEmptyNotNil(t *testing.T) {
	out := chartsDTO(map[string][]monitor.ChartSeries{})
	if out.Latency == nil || out.Concurrency == nil || out.KV == nil || out.Throughput == nil || out.StageTime == nil || out.Preemptions == nil {
		t.Fatalf("chartsDTO of an empty map produced a nil slice somewhere: %+v", out)
	}
}

// TestReplicaRowsDTONullsEverythingWhenNothingCollected pins the wire's
// null-vs-value rule for the replica table: an instance with every Has*
// flag false (no metrics collected at all yet) must render every numeric
// field, including its GPU sub-rows', as null - never a zero-valued 0.
func TestReplicaRowsDTONullsEverythingWhenNothingCollected(t *testing.T) {
	rows := []monitor.ReplicaRow{{
		InstanceUUID: "inst-1", State: "running", NodeName: "gpu3",
		GPUs: []monitor.ReplicaGPU{{Index: 0}},
	}}
	out := replicaRowsDTO(rows, map[string]string{})
	if len(out) != 1 {
		t.Fatalf("out = %+v, want 1 row", out)
	}
	row := out[0]
	if row.Running != nil || row.Waiting != nil || row.KVUsagePct != nil || row.KVHitRatePct != nil || row.PromptTokS != nil || row.GenerationTokS != nil {
		t.Fatalf("replica row = %+v, want every metric nil", row)
	}
	if row.InstanceUUID != "inst-1" || row.State != "running" || row.NodeName != "gpu3" {
		t.Fatalf("replica row identity fields = %+v", row)
	}
	if len(row.GPUIndexes) != 1 || row.GPUIndexes[0] != 0 {
		t.Fatalf("gpu_indexes = %+v, want [0]", row.GPUIndexes)
	}
	if len(row.GPUs) != 1 {
		t.Fatalf("gpus = %+v, want 1", row.GPUs)
	}
	gpu := row.GPUs[0]
	if gpu.UtilPct != nil || gpu.TemperatureC != nil || gpu.MemBWPct != nil || gpu.PowerW != nil {
		t.Fatalf("gpu sub-row = %+v, want every field nil", gpu)
	}
	if gpu.Index != 0 {
		t.Fatalf("gpu.Index = %d, want 0", gpu.Index)
	}
}

// TestReplicaRowsDTOInstanceStateOverridesRawState proves the
// instanceState override (deploymentapi's own rolled-up
// running/asleep/starting/failed/stopped classification) wins over the raw
// monitor.ReplicaRow.State when present, and falls back to the raw state
// only when the override map has nothing for that instance.
func TestReplicaRowsDTOInstanceStateOverridesRawState(t *testing.T) {
	rows := []monitor.ReplicaRow{
		{InstanceUUID: "overridden", State: "running"},
		{InstanceUUID: "not-overridden", State: "running"},
	}
	out := replicaRowsDTO(rows, map[string]string{"overridden": "asleep"})
	byUUID := map[string]string{}
	for _, r := range out {
		byUUID[r.InstanceUUID] = r.State
	}
	if byUUID["overridden"] != "asleep" {
		t.Fatalf("overridden state = %q, want asleep", byUUID["overridden"])
	}
	if byUUID["not-overridden"] != "running" {
		t.Fatalf("not-overridden state = %q, want the raw state running", byUUID["not-overridden"])
	}
}

// TestReplicaRowsDTORoundsNumericFieldsToOneDecimal proves every numeric
// replica/GPU field is rounded to one decimal, not just nulled when
// uncollected (TestReplicaRowsDTONullsEverythingWhenNothingCollected covers
// that half).
func TestReplicaRowsDTORoundsNumericFieldsToOneDecimal(t *testing.T) {
	rows := []monitor.ReplicaRow{{
		InstanceUUID: "inst-1", State: "running", NodeName: "gpu3",
		HasRunning: true, Running: 2.0500000000000003,
		HasWaiting: true, Waiting: 1.0499999999999998,
		HasKVUsage: true, KVUsage: 0.409999999999999, // *100 -> 40.9999999999999
		HasKVHitRate: true, KVHitRate: 0.850000000000001, // *100 -> 85.0000000000001
		HasTokens: true, PromptTokensPerSec: 10.05000000000001, GenTokensPerSec: 4.049999999999999,
		GPUs: []monitor.ReplicaGPU{{
			Index: 0, HasUtil: true, UtilPct: 55.049999999999997,
			HasTemp: true, TempC: 61.050000000000004,
			HasMemBW: true, MemBWPct: 30.949999999999999,
			HasPower: true, PowerW: 244.05000000000001,
		}},
	}}
	out := replicaRowsDTO(rows, map[string]string{})
	if len(out) != 1 {
		t.Fatalf("out = %+v, want 1 row", out)
	}
	row := out[0]
	if row.Running == nil || *row.Running != 2.1 {
		t.Fatalf("Running = %v, want 2.1 (rounded)", row.Running)
	}
	if row.Waiting == nil || *row.Waiting != 1.0 {
		t.Fatalf("Waiting = %v, want 1.0 (rounded)", row.Waiting)
	}
	if row.KVUsagePct == nil || *row.KVUsagePct != 41.0 {
		t.Fatalf("KVUsagePct = %v, want 41.0 (rounded)", row.KVUsagePct)
	}
	if row.KVHitRatePct == nil || *row.KVHitRatePct != 85.0 {
		t.Fatalf("KVHitRatePct = %v, want 85.0 (rounded)", row.KVHitRatePct)
	}
	if row.PromptTokS == nil || *row.PromptTokS != 10.1 {
		t.Fatalf("PromptTokS = %v, want 10.1 (rounded)", row.PromptTokS)
	}
	if row.GenerationTokS == nil || *row.GenerationTokS != 4.0 {
		t.Fatalf("GenerationTokS = %v, want 4.0 (rounded)", row.GenerationTokS)
	}
	if len(row.GPUs) != 1 {
		t.Fatalf("gpus = %+v, want 1", row.GPUs)
	}
	gpu := row.GPUs[0]
	if gpu.UtilPct == nil || *gpu.UtilPct != 55.1 {
		t.Fatalf("UtilPct = %v, want 55.1 (rounded)", gpu.UtilPct)
	}
	if gpu.TemperatureC == nil || *gpu.TemperatureC != 61.1 {
		t.Fatalf("TemperatureC = %v, want 61.1 (rounded)", gpu.TemperatureC)
	}
	if gpu.MemBWPct == nil || *gpu.MemBWPct != 31.0 {
		t.Fatalf("MemBWPct = %v, want 31.0 (rounded)", gpu.MemBWPct)
	}
	if gpu.PowerW == nil || *gpu.PowerW != 244.1 {
		t.Fatalf("PowerW = %v, want 244.1 (rounded)", gpu.PowerW)
	}
}

// TestBuildDeploymentReplicaGPUPower is GWKS-144's end-to-end regression at
// the wire boundary: a node that collects gpuworks_gpu_power_watts must
// reach the replica table's power_w as that card's mean draw over the
// diagnosis window (100/200/300 W here, so a MaxOf reduction - what
// temperature wants, and what would report the 300 W peak - fails this),
// rounded like every other GPU column. A card with no power series of its
// own stays null rather than a fabricated 0 W.
func TestBuildDeploymentReplicaGPUPower(t *testing.T) {
	store := newTestStore(t)
	inv := monitor.Inventory{
		Deployments: []monitor.DeploymentRef{{ID: 7, Name: "svc"}},
		Instances:   []monitor.InstanceRef{{UUID: "i1", DeploymentID: 7, NodeUUID: "n1", GPUIndexes: []int{0, 1}}},
	}
	svc := New(Deps{TSDB: store, Inventory: inventoryFunc(inv), Now: func() time.Time { return baseTime }})
	// Only GPU 0's power was collected; GPU 1 has no such series at all.
	labels := map[string]string{"node": "n1", "gpu": "0"}
	writeSample(t, store, "gpuworks_gpu_power_watts", labels, 100, baseTime.Add(-20*time.Second))
	writeSample(t, store, "gpuworks_gpu_power_watts", labels, 200, baseTime.Add(-10*time.Second))
	writeSample(t, store, "gpuworks_gpu_power_watts", labels, 300, baseTime)

	out, err := svc.Deployment(context.Background(), 7, time.Hour)
	if err != nil {
		t.Fatalf("Deployment: %v", err)
	}
	if len(out.Replicas) != 1 || len(out.Replicas[0].GPUs) != 2 {
		t.Fatalf("replicas = %+v, want one row with two GPU sub-rows", out.Replicas)
	}
	gpus := out.Replicas[0].GPUs
	if gpus[0].PowerW == nil || *gpus[0].PowerW != 200 {
		t.Fatalf("GPU 0 power_w = %v, want 200 (the window mean of 100/200/300)", gpus[0].PowerW)
	}
	if gpus[1].PowerW != nil {
		t.Fatalf("GPU 1 power_w = %v, want null (no power series was collected for it)", *gpus[1].PowerW)
	}
}

// TestBuildDeploymentAlignmentAndTTFTBreakdown is the end-to-end phase2
// coverage: proxy accounting + engine success counter + TTFT/queue/prefill
// quantile gauges all feed into the deployment page's alignment row and
// TTFT breakdown bar, exactly as the wire contract §3.2 describes.
func TestBuildDeploymentAlignmentAndTTFTBreakdown(t *testing.T) {
	store := newTestStore(t)
	inv := monitor.Inventory{
		Deployments: []monitor.DeploymentRef{{ID: 1, Name: "svc"}},
		Instances:   []monitor.InstanceRef{{UUID: "i1", DeploymentID: 1}},
	}
	svc := New(Deps{TSDB: store, Inventory: inventoryFunc(inv), Now: func() time.Time { return baseTime }})

	classLabels := map[string]string{"deployment": "1", "class": "2xx"}
	writeSample(t, store, "gpuworks_proxy_requests_total", classLabels, 0, baseTime.Add(-10*time.Second))
	writeSample(t, store, "gpuworks_proxy_requests_total", classLabels, 100, baseTime)
	successLabels := map[string]string{"deployment": "1", "instance": "i1"}
	writeSample(t, store, "gpuworks_infer_requests_success_total", successLabels, 0, baseTime.Add(-10*time.Second))
	writeSample(t, store, "gpuworks_infer_requests_success_total", successLabels, 90, baseTime)

	ttftLabels := map[string]string{"deployment": "1", "instance": "i1", "quantile": "0.95"}
	writeSample(t, store, "gpuworks_infer_ttft_seconds", ttftLabels, 2.5, baseTime)
	queueLabels := map[string]string{"deployment": "1", "instance": "i1", "quantile": "0.95"}
	writeSample(t, store, "gpuworks_infer_queue_time_seconds", queueLabels, 0.5, baseTime)
	writeSample(t, store, "gpuworks_infer_prefill_time_seconds", queueLabels, 1.0, baseTime)

	out, err := svc.Deployment(context.Background(), 1, time.Hour)
	if err != nil {
		t.Fatalf("Deployment: %v", err)
	}
	if out.Alignment == nil {
		t.Fatal("Alignment = nil, want a populated row (proxy traffic exists)")
	}
	if out.Alignment.ProxyRequests == 0 {
		t.Errorf("Alignment.ProxyRequests = 0, want > 0")
	}
	if out.Alignment.EngineCompleted == 0 {
		t.Errorf("Alignment.EngineCompleted = 0, want > 0 (success counter has data)")
	}
	if out.TTFTBreakdown == nil {
		t.Fatal("TTFTBreakdown = nil, want populated (TTFT collected)")
	}
	if out.TTFTBreakdown.TotalMs != 2500 {
		t.Errorf("TTFTBreakdown.TotalMs = %v, want 2500", out.TTFTBreakdown.TotalMs)
	}
	if out.TTFTBreakdown.QueueMs != 500 || out.TTFTBreakdown.PrefillMs != 1000 {
		t.Errorf("TTFTBreakdown = %+v, want queue=500 prefill=1000", out.TTFTBreakdown)
	}
	if got, want := out.TTFTBreakdown.FirstTokenMs, 1000.0; got != want {
		t.Errorf("TTFTBreakdown.FirstTokenMs = %v, want %v (2500-500-1000)", got, want)
	}
	if out.TTFTBreakdown.Approximate {
		t.Error("TTFTBreakdown.Approximate = true, want false (queue+prefill comfortably fits under total)")
	}
}

// TestBuildDeploymentAlignmentNilWithoutProxyTraffic proves the alignment
// row stays nil (not a zero-valued struct) for a deployment with no proxy
// accounting data at all.
func TestBuildDeploymentAlignmentNilWithoutProxyTraffic(t *testing.T) {
	store := newTestStore(t)
	inv := monitor.Inventory{
		Deployments: []monitor.DeploymentRef{{ID: 2, Name: "svc"}},
		Instances:   []monitor.InstanceRef{{UUID: "i1", DeploymentID: 2}},
	}
	svc := New(Deps{TSDB: store, Inventory: inventoryFunc(inv), Now: func() time.Time { return baseTime }})
	out, err := svc.Deployment(context.Background(), 2, time.Hour)
	if err != nil {
		t.Fatalf("Deployment: %v", err)
	}
	if out.Alignment != nil {
		t.Errorf("Alignment = %+v, want nil without proxy traffic", out.Alignment)
	}
	if out.TTFTBreakdown != nil {
		t.Errorf("TTFTBreakdown = %+v, want nil without TTFT data", out.TTFTBreakdown)
	}
}

// TestTTFTBreakdownDTOScalesDownWhenQueuePlusPrefillExceedsTotal proves
// item 7: when queue+prefill alone already exceeds TTFT's own p95, both are
// scaled down proportionally so all three segments sum to TotalMs exactly,
// rather than letting the bar chart's segments overflow their own total.
func TestTTFTBreakdownDTOScalesDownWhenQueuePlusPrefillExceedsTotal(t *testing.T) {
	w := monitor.Window{
		HasTTFT: true, TTFTP95Sec: 1.0, // total = 1000ms
		HasQueueTime: true, QueueTimeP95Sec: 0.8, // 800ms
		HasPrefillTime: true, PrefillTimeP95Sec: 0.8, // 800ms; sum 1600 > 1000
	}
	out := ttftBreakdownDTO(w)
	if out == nil {
		t.Fatal("ttftBreakdownDTO = nil")
	}
	if !out.Approximate {
		t.Error("Approximate = false, want true (queue+prefill needed scaling)")
	}
	// scale = 1000/1600 = 0.625: queue=500, prefill=500.
	if out.QueueMs != 500 || out.PrefillMs != 500 {
		t.Fatalf("QueueMs/PrefillMs = %v/%v, want 500/500 (proportionally scaled)", out.QueueMs, out.PrefillMs)
	}
	if got, want := out.QueueMs+out.PrefillMs+out.FirstTokenMs, out.TotalMs; got != want {
		t.Fatalf("segments sum to %v, want exactly TotalMs %v", got, want)
	}
	if out.FirstTokenMs != 0 {
		t.Fatalf("FirstTokenMs = %v, want 0 (queue+prefill scaled to consume the whole total)", out.FirstTokenMs)
	}
}

// TestTTFTBreakdownDTOSnapsFloatDustToZero is item 16's regression case:
// proportional scaling by a non-terminating fraction (here 1000/3000 =
// 0.3333...) leaves FirstTokenMs a few float64 ULPs away from exactly 0
// (e.g. 1.1e-13) instead of exactly 0 - roundMsSnapZero must snap that to a
// clean 0 rather than let it reach the wire as a false-precision non-zero
// millisecond value, and QueueMs/PrefillMs must come out rounded to one
// decimal.
func TestTTFTBreakdownDTOSnapsFloatDustToZero(t *testing.T) {
	w := monitor.Window{
		HasTTFT: true, TTFTP95Sec: 1.0, // total = 1000ms
		HasQueueTime: true, QueueTimeP95Sec: 1.0, // 1000ms
		HasPrefillTime: true, PrefillTimeP95Sec: 2.0, // 2000ms; sum 3000 > 1000
	}
	out := ttftBreakdownDTO(w)
	if out == nil {
		t.Fatal("ttftBreakdownDTO = nil")
	}
	if out.FirstTokenMs != 0 {
		t.Fatalf("FirstTokenMs = %v, want exactly 0 (float dust snapped)", out.FirstTokenMs)
	}
	// scale = 1000/3000 = 0.3333...: queue=333.3, prefill=666.7, rounded to
	// one decimal.
	if out.QueueMs != 333.3 {
		t.Errorf("QueueMs = %v, want 333.3 (rounded to one decimal)", out.QueueMs)
	}
	if out.PrefillMs != 666.7 {
		t.Errorf("PrefillMs = %v, want 666.7 (rounded to one decimal)", out.PrefillMs)
	}
}

// TestTTFTBreakdownDTORoundsTotalMs proves TotalMs (TTFT's own p95 scaled to
// ms) is rounded to one decimal like the three segments it accompanies - a
// real server observed a raw *1000 conversion like 2349.9999999999995 on
// the wire before this rounding was added.
func TestTTFTBreakdownDTORoundsTotalMs(t *testing.T) {
	w := monitor.Window{HasTTFT: true, TTFTP95Sec: 2.3499999999999996}
	out := ttftBreakdownDTO(w)
	if out == nil {
		t.Fatal("ttftBreakdownDTO = nil")
	}
	if out.TotalMs != 2350.0 {
		t.Fatalf("TotalMs = %v, want 2350.0 (rounded)", out.TotalMs)
	}
}

func TestRoundMsSnapZero(t *testing.T) {
	cases := []struct {
		in, want float64
	}{
		{0, 0},
		{1.1e-13, 0},
		{-1.1e-13, 0},
		{0.049, 0},
		{-0.049, 0},
		{0.05, 0.1},
		{-0.05, -0.1},
		{333.3333333333, 333.3},
		{666.6666666667, 666.7},
	}
	for _, c := range cases {
		if got := roundMsSnapZero(c.in); got != c.want {
			t.Errorf("roundMsSnapZero(%v) = %v, want %v", c.in, got, c.want)
		}
	}
}

// TestTTFTBreakdownDTONotApproximateWhenSegmentsFit is the scaling path's
// negative case: queue+prefill comfortably under total needs no adjustment
// and Approximate stays false.
func TestTTFTBreakdownDTONotApproximateWhenSegmentsFit(t *testing.T) {
	w := monitor.Window{
		HasTTFT: true, TTFTP95Sec: 2.5,
		HasQueueTime: true, QueueTimeP95Sec: 0.5,
		HasPrefillTime: true, PrefillTimeP95Sec: 1.0,
	}
	out := ttftBreakdownDTO(w)
	if out == nil || out.Approximate {
		t.Fatalf("ttftBreakdownDTO = %+v, want Approximate=false", out)
	}
	if out.QueueMs != 500 || out.PrefillMs != 1000 || out.FirstTokenMs != 1000 {
		t.Fatalf("segments = %+v, want queue=500 prefill=1000 first_token=1000 (unscaled)", out)
	}
}

func TestTTFTBreakdownDTONilWithoutTTFT(t *testing.T) {
	if out := ttftBreakdownDTO(monitor.Window{}); out != nil {
		t.Fatalf("ttftBreakdownDTO = %+v, want nil", out)
	}
}

// TestLogInstancesIncludesLiveInstancesDeduped proves logInstances starts
// from the deployment's live replicas, deduplicating any repeated UUID and
// ignoring headless workers with no UUID at all, with no event timeline
// wired.
func TestLogInstancesIncludesLiveInstancesDeduped(t *testing.T) {
	svc := New(Deps{Now: func() time.Time { return baseTime }})
	live := []monitor.InstanceRef{{UUID: "i1"}, {UUID: "i2"}, {UUID: "i1"}, {UUID: ""}}
	out, err := svc.logInstances(context.Background(), 1, live, baseTime, time.Hour)
	if err != nil {
		t.Fatalf("logInstances: %v", err)
	}
	if len(out) != 2 || out[0] != "i1" || out[1] != "i2" {
		t.Fatalf("logInstances = %+v, want [i1 i2] (deduped, empty UUID dropped)", out)
	}
}

// TestLogInstancesAddsRecentlyStoppedFromEventsWithinWindow proves an
// instance no longer in the live set (store.DeleteInstance already removed
// its row - see logInstances' doc comment) is still surfaced when its event
// timeline entry falls inside the requested window, and is not duplicated
// when it also appears live.
func TestLogInstancesAddsRecentlyStoppedFromEventsWithinWindow(t *testing.T) {
	live := []monitor.InstanceRef{{UUID: "i1"}}
	svc := New(Deps{
		Now: func() time.Time { return baseTime },
		DeploymentEvents: func(_ context.Context, _ uint, before uint, limit int) ([]EventRef, error) {
			if before != 0 {
				return nil, nil
			}
			return []EventRef{
				{ID: 3, InstanceUUID: "i1", CreatedAtMS: baseTime.Add(-time.Minute).UnixMilli()},            // already live - must not duplicate
				{ID: 2, InstanceUUID: "i-stopped", CreatedAtMS: baseTime.Add(-2 * time.Minute).UnixMilli()}, // recently stopped, inside window
				{ID: 1, InstanceUUID: "i-ancient", CreatedAtMS: baseTime.Add(-2 * time.Hour).UnixMilli()},   // outside the 1h window
			}, nil
		},
	})
	out, err := svc.logInstances(context.Background(), 1, live, baseTime, time.Hour)
	if err != nil {
		t.Fatalf("logInstances: %v", err)
	}
	if len(out) != 2 || out[0] != "i1" || out[1] != "i-stopped" {
		t.Fatalf("logInstances = %+v, want [i1 i-stopped]", out)
	}
}

// TestLogInstancesCapsAtMax proves the list never grows past maxLogInstances
// even when live instances plus event history would otherwise exceed it.
func TestLogInstancesCapsAtMax(t *testing.T) {
	live := make([]monitor.InstanceRef, maxLogInstances-1)
	for i := range live {
		live[i] = monitor.InstanceRef{UUID: fmt.Sprintf("live-%d", i)}
	}
	svc := New(Deps{
		Now: func() time.Time { return baseTime },
		DeploymentEvents: func(_ context.Context, _ uint, before uint, limit int) ([]EventRef, error) {
			if before != 0 {
				return nil, nil
			}
			evs := make([]EventRef, 10)
			for i := range evs {
				evs[i] = EventRef{ID: uint(10 - i), InstanceUUID: fmt.Sprintf("stopped-%d", i), CreatedAtMS: baseTime.UnixMilli()}
			}
			return evs, nil
		},
	})
	out, err := svc.logInstances(context.Background(), 1, live, baseTime, time.Hour)
	if err != nil {
		t.Fatalf("logInstances: %v", err)
	}
	if len(out) != maxLogInstances {
		t.Fatalf("logInstances returned %d entries, want exactly the %d cap", len(out), maxLogInstances)
	}
}

// TestBuildDeploymentPopulatesLogInstances is the end-to-end check: the
// /monitor/deployments/{id} response's log_instances field carries the
// live replica plus a recently stopped one from the event timeline, never
// null even when nothing merges in.
func TestBuildDeploymentPopulatesLogInstances(t *testing.T) {
	store := newTestStore(t)
	inv := monitor.Inventory{
		Deployments: []monitor.DeploymentRef{{ID: 7, Name: "svc"}},
		Instances:   []monitor.InstanceRef{{UUID: "i1", DeploymentID: 7}},
	}
	svc := New(Deps{
		TSDB: store, Inventory: inventoryFunc(inv), Now: func() time.Time { return baseTime },
		DeploymentEvents: func(_ context.Context, _ uint, before uint, limit int) ([]EventRef, error) {
			if before != 0 {
				return nil, nil
			}
			return []EventRef{{ID: 1, InstanceUUID: "i-stopped", CreatedAtMS: baseTime.Add(-time.Minute).UnixMilli()}}, nil
		},
	})
	out, err := svc.Deployment(context.Background(), 7, time.Hour)
	if err != nil {
		t.Fatalf("Deployment: %v", err)
	}
	if len(out.LogInstances) != 2 || out.LogInstances[0] != "i1" || out.LogInstances[1] != "i-stopped" {
		t.Fatalf("LogInstances = %+v, want [i1 i-stopped]", out.LogInstances)
	}
}

// TestBuildAlignmentSplitsClientAndServerErrors proves client_errors (4xx +
// 429) and server_errors (5xx + asleep_503) are correctly partitioned - not
// only that "some errors" moved money into one bucket.
func TestBuildAlignmentSplitsClientAndServerErrors(t *testing.T) {
	store := newTestStore(t)
	inv := monitor.Inventory{
		Deployments: []monitor.DeploymentRef{{ID: 3, Name: "svc"}},
		Instances:   []monitor.InstanceRef{{UUID: "i1", DeploymentID: 3}},
	}
	svc := New(Deps{TSDB: store, Inventory: inventoryFunc(inv), Now: func() time.Time { return baseTime }})

	start := baseTime.Add(-70 * time.Second)
	for i := 0; i < 8; i++ {
		at := start.Add(time.Duration(i) * 10 * time.Second)
		n := float64(i + 1)
		// Per tick: 70 2xx, 10 4xx, 5 429, 10 5xx, 5 asleep_503 (100 total).
		writeSample(t, store, "gpuworks_proxy_requests_total", map[string]string{"deployment": "3", "class": "2xx"}, 70*n, at)
		writeSample(t, store, "gpuworks_proxy_requests_total", map[string]string{"deployment": "3", "class": "4xx"}, 10*n, at)
		writeSample(t, store, "gpuworks_proxy_requests_total", map[string]string{"deployment": "3", "class": "429"}, 5*n, at)
		writeSample(t, store, "gpuworks_proxy_requests_total", map[string]string{"deployment": "3", "class": "5xx"}, 10*n, at)
		writeSample(t, store, "gpuworks_proxy_requests_total", map[string]string{"deployment": "3", "class": "asleep_503"}, 5*n, at)
		writeSample(t, store, "gpuworks_infer_requests_success_total", map[string]string{"deployment": "3", "instance": "i1"}, 70*n, at)
	}

	out, err := svc.Deployment(context.Background(), 3, time.Hour)
	if err != nil {
		t.Fatalf("Deployment: %v", err)
	}
	if out.Alignment == nil {
		t.Fatal("Alignment = nil, want populated")
	}
	// client_errors = (4xx+429)/total * proxy_requests = 15% of proxy_requests;
	// server_errors = (5xx+asleep)/total * proxy_requests = 15% of proxy_requests.
	wantClient := out.Alignment.ProxyRequests * 15 / 100
	wantServer := out.Alignment.ProxyRequests * 15 / 100
	if diff := out.Alignment.ClientErrors - wantClient; diff < -1 || diff > 1 {
		t.Errorf("ClientErrors = %d, want ~%d (4xx+429 = 15%% of proxy_requests)", out.Alignment.ClientErrors, wantClient)
	}
	if diff := out.Alignment.ServerErrors - wantServer; diff < -1 || diff > 1 {
		t.Errorf("ServerErrors = %d, want ~%d (5xx+asleep_503 = 15%% of proxy_requests)", out.Alignment.ServerErrors, wantServer)
	}
}
