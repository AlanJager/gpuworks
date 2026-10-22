package monitor

import (
	"context"
	"math"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

func gatewayLabels(extra map[string]string) map[string]string {
	labels := map[string]string{"deployment": "1", "node": "server"}
	for k, v := range extra {
		labels[k] = v
	}
	return labels
}

func strataInventory() Inventory {
	return Inventory{
		Deployments: []DeploymentRef{{ID: 1, Name: "strata", Engine: "callable", ReplicasDesired: 1}},
		Instances:   []InstanceRef{{UUID: "s1", DeploymentID: 1, NodeUUID: "node-1", State: proto.InstRunning}},
		Nodes:       []NodeRef{{UUID: "node-1", Name: "node-1", State: "online"}},
	}
}

func near(a, b float64) bool { return math.Abs(a-b) < 1e-9 }

// An engine without metrics: every signal the gateway measures is filled
// from it, in the engine fields' units, and its source is recorded.
func TestGatewayStandsInForAnEngineWithoutMetrics(t *testing.T) {
	store := newTestStore(t)
	start := baseTime.Add(-4 * time.Minute)
	step := 10 * time.Second
	ticks := 24
	flat := func(v float64) []float64 {
		out := make([]float64, ticks)
		for i := range out {
			out[i] = v
		}
		return out
	}
	ramp := func(perTick float64) []float64 {
		out := make([]float64, ticks)
		for i := range out {
			out[i] = perTick * float64(i)
		}
		return out
	}
	writeSeries(t, store, "gpuworks_proxy_ttft_ms", gatewayLabels(map[string]string{"quantile": "0.5"}), start, step, flat(300))
	writeSeries(t, store, "gpuworks_proxy_ttft_ms", gatewayLabels(map[string]string{"quantile": "0.95"}), start, step, flat(800))
	writeSeries(t, store, "gpuworks_proxy_ttft_ms", gatewayLabels(map[string]string{"quantile": "0.99"}), start, step, flat(900))
	writeSeries(t, store, "gpuworks_proxy_request_duration_ms", gatewayLabels(map[string]string{"quantile": "0.95"}), start, step, flat(4000))
	writeSeries(t, store, "gpuworks_proxy_prompt_tokens_total", gatewayLabels(nil), start, step, ramp(100))
	writeSeries(t, store, "gpuworks_proxy_completion_tokens_total", gatewayLabels(nil), start, step, ramp(500))
	writeSeries(t, store, "gpuworks_proxy_requests_inflight", gatewayLabels(nil), start, step, flat(3))
	instances := strataInventory().Instances

	series, err := FetchWindowSeries(store, 1, instances, baseTime, baseTime.Add(-DiagnosisWindow), WindowStep.Milliseconds())
	if err != nil {
		t.Fatal(err)
	}
	for _, signal := range []Signal{SignalTTFT, SignalE2E, SignalPromptTokens, SignalGenerationTokens, SignalRunning} {
		if series.Sources[signal] != SourceGateway {
			t.Errorf("%s source = %q, want gateway", signal, series.Sources[signal])
		}
	}
	w := ComputeWindow(series, WindowParams{Now: baseTime, Span: DiagnosisWindow})
	if !w.HasTTFT || !near(w.TTFTP95Sec, 0.8) || !near(w.TTFTP50Sec, 0.3) || !near(w.TTFTP99Sec, 0.9) {
		t.Errorf("ttft = has %v p50 %v p95 %v p99 %v, want seconds from the gateway's milliseconds", w.HasTTFT, w.TTFTP50Sec, w.TTFTP95Sec, w.TTFTP99Sec)
	}
	if !w.HasE2E || !near(w.E2EP95Sec, 4) {
		t.Errorf("e2e = %v %v", w.HasE2E, w.E2EP95Sec)
	}
	if !w.HasTokens || math.Abs(w.GenTokensPerSec-50) > 0.5 || math.Abs(w.PromptTokensPerSec-10) > 0.5 {
		t.Errorf("tokens = %v prompt %v/s generation %v/s, want 10 and 50", w.HasTokens, w.PromptTokensPerSec, w.GenTokensPerSec)
	}
	if !w.HasRunning || !near(w.RunningAvg, 3) {
		t.Errorf("running = %v %v, want the in-flight 3", w.HasRunning, w.RunningAvg)
	}
	// What only an engine reports stays absent, and so does a TPOT the
	// gateway never measured.
	if w.HasWaiting || w.HasKVUsage || w.HasTPOT {
		t.Errorf("engine-only signals filled: waiting %v kv %v tpot %v", w.HasWaiting, w.HasKVUsage, w.HasTPOT)
	}

	deps := Deps{TSDB: store, Inventory: inventoryFunc(strataInventory()), Now: func() time.Time { return baseTime }}
	snap, err := BuildService(context.Background(), deps, 1, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	names := map[string]bool{}
	for _, chart := range []string{"latency", "throughput", "concurrency"} {
		for _, s := range snap.Charts[chart] {
			if len(s.Points) > 0 {
				names[chart+"/"+s.Name] = true
			}
		}
	}
	for _, want := range []string{"latency/ttft_p95", "latency/e2e_p95", "throughput/generation_tokens_per_sec", "throughput/prompt_tokens_per_sec", "concurrency/running"} {
		if !names[want] {
			t.Errorf("chart line %s missing; have %v", want, names)
		}
	}
	if len(snap.Charts["kv"]) != 0 || snap.Sources[SignalTTFT] != SourceGateway {
		t.Errorf("kv = %+v, sources = %v", snap.Charts["kv"], snap.Sources)
	}

	cluster, err := BuildCluster(context.Background(), deps, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(cluster.Services) != 1 || !cluster.Services[0].HasTTFT || !cluster.Services[0].HasTokens {
		t.Fatalf("service row = %+v", cluster.Services)
	}
	if cluster.Signals.OutputTokensPerSec < 49 {
		t.Errorf("fleet output tokens = %v, want the gateway's 50/s", cluster.Signals.OutputTokensPerSec)
	}
	// The sparklines beside those values draw the same source.
	if len(cluster.Signals.TTFTP95Series) == 0 || len(cluster.Signals.PromptTokensPerSecSeries) == 0 {
		t.Errorf("fleet sparklines without the gateway: ttft %d points, prompt %d points", len(cluster.Signals.TTFTP95Series), len(cluster.Signals.PromptTokensPerSecSeries))
	}
}

// When the engine reports a signal, the gateway's never replaces it, even
// where both exist: the engine's TTFT excludes the queue and the network.
func TestEngineSignalsWinOverTheGateway(t *testing.T) {
	store := newTestStore(t)
	start := baseTime.Add(-4 * time.Minute)
	step := 10 * time.Second
	engine := map[string]string{"instance": "s1", "deployment": "1"}
	for i := 0; i < 24; i++ {
		at := start.Add(time.Duration(i) * step)
		writeSample(t, store, "gpuworks_infer_ttft_seconds", map[string]string{"instance": "s1", "deployment": "1", "quantile": "0.95"}, 0.2, at)
		writeSample(t, store, "gpuworks_proxy_ttft_ms", gatewayLabels(map[string]string{"quantile": "0.95"}), 900, at)
		writeSample(t, store, "gpuworks_infer_requests_running", engine, 2, at)
		writeSample(t, store, "gpuworks_proxy_requests_inflight", gatewayLabels(nil), 5, at)
	}
	instances := strataInventory().Instances
	series, err := FetchWindowSeries(store, 1, instances, baseTime, baseTime.Add(-DiagnosisWindow), WindowStep.Milliseconds())
	if err != nil {
		t.Fatal(err)
	}
	if series.Sources[SignalTTFT] != SourceEngine || series.Sources[SignalRunning] != SourceEngine {
		t.Fatalf("sources = %v", series.Sources)
	}
	if _, ok := series.Sources[SignalPromptTokens]; ok {
		t.Fatalf("a signal neither source has is recorded: %v", series.Sources)
	}
	w := ComputeWindow(series, WindowParams{Now: baseTime, Span: DiagnosisWindow})
	if !near(w.TTFTP95Sec, 0.2) || !near(w.RunningAvg, 2) {
		t.Fatalf("ttft p95 %v running %v, want the engine's 0.2s and 2", w.TTFTP95Sec, w.RunningAvg)
	}
}

// An engine that exports no TPOT and no phase histograms (TensorFold): the
// gateway's TPOT and the phase times its replies reported fill the window,
// the service page's TPOT and stage-time charts and the fleet's TPOT; a phase
// the replies never carried (TensorFold reports no queue time) stays absent.
// Where the engine does report one, the engine's wins.
func TestGatewayStandsInForTPOTAndPhaseTimes(t *testing.T) {
	store := newTestStore(t)
	start := baseTime.Add(-4 * time.Minute)
	step := 10 * time.Second
	for i := 0; i < 24; i++ {
		at := start.Add(time.Duration(i) * step)
		for metric, value := range map[string]float64{
			"gpuworks_proxy_tpot_ms":             40,
			"gpuworks_proxy_reported_prefill_ms": 250,
			"gpuworks_proxy_reported_decode_ms":  3000,
		} {
			writeSample(t, store, metric, gatewayLabels(map[string]string{"quantile": "0.95"}), value, at)
			writeSample(t, store, metric, gatewayLabels(map[string]string{"quantile": "0.99"}), value*1.5, at)
		}
		// The engine reports its own TTFT, so only the missing signals are the
		// gateway's.
		writeSample(t, store, "gpuworks_infer_ttft_seconds", map[string]string{"instance": "s1", "deployment": "1", "quantile": "0.95"}, 0.2, at)
	}
	instances := strataInventory().Instances
	series, err := FetchWindowSeries(store, 1, instances, baseTime, baseTime.Add(-DiagnosisWindow), WindowStep.Milliseconds())
	if err != nil {
		t.Fatal(err)
	}
	for signal, want := range map[Signal]Source{SignalTTFT: SourceEngine, SignalTPOT: SourceGateway, SignalPrefillTime: SourceGateway, SignalDecodeTime: SourceGateway} {
		if series.Sources[signal] != want {
			t.Errorf("%s source = %q, want %q", signal, series.Sources[signal], want)
		}
	}
	if _, ok := series.Sources[SignalQueueTime]; ok {
		t.Errorf("queue time has a source though nothing reported it: %v", series.Sources)
	}
	w := ComputeWindow(series, WindowParams{Now: baseTime, Span: DiagnosisWindow})
	if !w.HasTPOT || !near(w.TPOTP95Sec, 0.04) || !near(w.TPOTP99Sec, 0.06) {
		t.Errorf("tpot = has %v p95 %v p99 %v, want seconds from the gateway's 40/60 ms", w.HasTPOT, w.TPOTP95Sec, w.TPOTP99Sec)
	}
	if !w.HasPrefillTime || !near(w.PrefillTimeP95Sec, 0.25) || !w.HasDecodeTime || !near(w.DecodeTimeP95Sec, 3) || w.HasQueueTime {
		t.Errorf("phases = prefill %v %v decode %v %v queue %v", w.HasPrefillTime, w.PrefillTimeP95Sec, w.HasDecodeTime, w.DecodeTimeP95Sec, w.HasQueueTime)
	}

	deps := Deps{TSDB: store, Inventory: inventoryFunc(strataInventory()), Now: func() time.Time { return baseTime }}
	snap, err := BuildService(context.Background(), deps, 1, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	lines := map[string]float64{}
	for _, chart := range []string{"tpot", "stage_time"} {
		for _, s := range snap.Charts[chart] {
			if len(s.Points) > 0 {
				lines[chart+"/"+s.Name] = s.Points[len(s.Points)-1].Value
			}
		}
	}
	for line, want := range map[string]float64{"tpot/tpot_p95": 0.04, "tpot/tpot_p99": 0.06, "stage_time/prefill_ms": 0.25, "stage_time/decode_ms": 3} {
		if got, ok := lines[line]; !ok || !near(got, want) {
			t.Errorf("chart line %s = %v (present %v), want %v seconds", line, got, ok, want)
		}
	}
	if _, ok := lines["stage_time/queue_ms"]; ok {
		t.Errorf("a queue line was drawn from nothing: %v", lines)
	}

	cluster, err := BuildCluster(context.Background(), deps, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if !cluster.Signals.HasTPOT || cluster.Signals.TPOTP95Ms < 39 {
		t.Errorf("fleet tpot = %v %v ms, want the gateway's 40", cluster.Signals.HasTPOT, cluster.Signals.TPOTP95Ms)
	}

	// The engine's own TPOT replaces the gateway's.
	for i := 0; i < 24; i++ {
		writeSample(t, store, "gpuworks_infer_tpot_seconds", map[string]string{"instance": "s1", "deployment": "1", "quantile": "0.95"}, 0.01, start.Add(time.Duration(i)*step))
	}
	series, err = FetchWindowSeries(store, 1, instances, baseTime, baseTime.Add(-DiagnosisWindow), WindowStep.Milliseconds())
	if err != nil {
		t.Fatal(err)
	}
	if series.Sources[SignalTPOT] != SourceEngine {
		t.Fatalf("tpot source = %q with the engine reporting, want engine", series.Sources[SignalTPOT])
	}
}
