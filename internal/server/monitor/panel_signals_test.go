package monitor

import (
	"errors"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

func panelInstances() []InstanceRef {
	return []InstanceRef{{UUID: "i1", DeploymentID: 1, NodeUUID: "n1", State: proto.InstRunning}}
}

func engineLabels(instance string) map[string]string {
	return map[string]string{"deployment": "1", "instance": instance, "node": "n1"}
}

func ramp(ticks int, perTick float64) []float64 {
	out := make([]float64, ticks)
	for i := range out {
		out[i] = perTick * float64(i)
	}
	return out
}

func flatValues(ticks int, v float64) []float64 {
	out := make([]float64, ticks)
	for i := range out {
		out[i] = v
	}
	return out
}

func lastValue(t *testing.T, chart []ChartSeries, name string) (float64, ChartSeries) {
	t.Helper()
	for _, s := range chart {
		if s.Name == name && len(s.Points) > 0 {
			return s.Points[len(s.Points)-1].Value, s
		}
	}
	t.Fatalf("chart has no %q series with points: %+v", name, chart)
	return 0, ChartSeries{}
}

func panelWindow() (start time.Time, step time.Duration, ticks int) {
	return baseTime.Add(-4 * time.Minute), 10 * time.Second, 24
}

// The deployment the drawer showed empty: the engine reports running,
// waiting and cache usage but no token counters, while the gateway counts the
// tokens of the replies it forwarded. The token signals come from the
// gateway, the others stay the engine's, per signal.
func TestPanelSignalsTakeTokensFromTheGatewayWhenTheEngineReportsNone(t *testing.T) {
	store := newTestStore(t)
	start, step, ticks := panelWindow()
	writeSeries(t, store, "gpuworks_infer_requests_running", engineLabels("i1"), start, step, flatValues(ticks, 2))
	writeSeries(t, store, "gpuworks_infer_requests_waiting", engineLabels("i1"), start, step, flatValues(ticks, 1))
	writeSeries(t, store, "gpuworks_infer_gpu_cache_usage_percent", engineLabels("i1"), start, step, flatValues(ticks, 0.25))
	writeSeries(t, store, "gpuworks_proxy_prompt_tokens_total", gatewayLabels(nil), start, step, ramp(ticks, 100))
	writeSeries(t, store, "gpuworks_proxy_completion_tokens_total", gatewayLabels(nil), start, step, ramp(ticks, 500))
	writeSeries(t, store, "gpuworks_proxy_requests_inflight", gatewayLabels(nil), start, step, flatValues(ticks, 7))

	got, err := BuildPanelSignals(store, 1, panelInstances(), start.UnixMilli(), baseTime.UnixMilli(), step.Milliseconds())
	if err != nil {
		t.Fatal(err)
	}
	want := map[Signal]Source{SignalRunning: SourceEngine, SignalPromptTokens: SourceGateway, SignalGenerationTokens: SourceGateway}
	if len(got.Sources) != len(want) {
		t.Fatalf("sources = %v, want %v", got.Sources, want)
	}
	for signal, source := range want {
		if got.Sources[signal] != source {
			t.Errorf("source of %s = %q, want %q", signal, got.Sources[signal], source)
		}
	}
	gen, genSeries := lastValue(t, got.Charts["throughput"], "generation_tokens_per_sec")
	if !near(gen, 50) {
		t.Errorf("generation tok/s = %v, want the gateway's 50", gen)
	}
	if prompt, _ := lastValue(t, got.Charts["throughput"], "prompt_tokens_per_sec"); !near(prompt, 10) {
		t.Errorf("prompt tok/s = %v, want the gateway's 10", prompt)
	}
	if _, has := genSeries.Labels["instance"]; has {
		t.Errorf("a gateway series claims an instance: %v", genSeries.Labels)
	}
	// Running stays the engine's per-instance 2, not the gateway's in-flight 7.
	running, runningSeries := lastValue(t, got.Charts["concurrency"], "running")
	if running != 2 || runningSeries.Labels["instance"] != "i1" {
		t.Errorf("running = %v %v, want the engine's 2 for i1", running, runningSeries.Labels)
	}
	if waiting, _ := lastValue(t, got.Charts["concurrency"], "waiting"); waiting != 1 {
		t.Errorf("waiting = %v, want the engine's 1", waiting)
	}
	if usage, s := lastValue(t, got.Charts["kv"], "usage"); usage != 0.25 || s.Labels["instance"] != "i1" {
		t.Errorf("kv usage = %v %v, want the engine's 0.25 for i1", usage, s.Labels)
	}
}

// An engine that reports tokens wins even when the gateway counted some too:
// the engine's numbers are its own and stay per instance.
func TestPanelSignalsPreferTheEnginesTokens(t *testing.T) {
	store := newTestStore(t)
	start, step, ticks := panelWindow()
	writeSeries(t, store, "gpuworks_infer_requests_running", engineLabels("i1"), start, step, flatValues(ticks, 2))
	writeSeries(t, store, "gpuworks_infer_prompt_tokens_total", engineLabels("i1"), start, step, ramp(ticks, 200))
	writeSeries(t, store, "gpuworks_infer_generation_tokens_total", engineLabels("i1"), start, step, ramp(ticks, 300))
	writeSeries(t, store, "gpuworks_proxy_prompt_tokens_total", gatewayLabels(nil), start, step, ramp(ticks, 100))
	writeSeries(t, store, "gpuworks_proxy_completion_tokens_total", gatewayLabels(nil), start, step, ramp(ticks, 500))

	got, err := BuildPanelSignals(store, 1, panelInstances(), start.UnixMilli(), baseTime.UnixMilli(), step.Milliseconds())
	if err != nil {
		t.Fatal(err)
	}
	for _, signal := range panelSources {
		if got.Sources[signal] != SourceEngine {
			t.Errorf("source of %s = %q, want engine", signal, got.Sources[signal])
		}
	}
	gen, s := lastValue(t, got.Charts["throughput"], "generation_tokens_per_sec")
	if !near(gen, 30) || s.Labels["instance"] != "i1" {
		t.Errorf("generation tok/s = %v %v, want the engine's 30 for i1", gen, s.Labels)
	}
	if prompt, _ := lastValue(t, got.Charts["throughput"], "prompt_tokens_per_sec"); !near(prompt, 20) {
		t.Errorf("prompt tok/s = %v, want the engine's 20", prompt)
	}
}

// With nothing from either source there are no series and no source: the
// panel shows it as missing, never as zero.
func TestPanelSignalsAreEmptyWithoutAnySource(t *testing.T) {
	got, err := BuildPanelSignals(newTestStore(t), 1, panelInstances(), baseTime.Add(-time.Hour).UnixMilli(), baseTime.UnixMilli(), 30_000)
	if err != nil {
		t.Fatal(err)
	}
	if len(got.Sources) != 0 {
		t.Errorf("sources = %v, want none", got.Sources)
	}
	for name, chart := range got.Charts {
		if len(chart) != 0 {
			t.Errorf("chart %s = %+v, want empty", name, chart)
		}
	}
}

// A deployment whose engine reports everything never reads the gateway's
// series: the polling path costs the same as before it could fall back.
func TestPanelSignalsSkipTheGatewayWhenTheEngineReportsEverything(t *testing.T) {
	inner := newTestStore(t)
	start, step, ticks := panelWindow()
	writeSeries(t, inner, "gpuworks_infer_requests_running", engineLabels("i1"), start, step, flatValues(ticks, 2))
	writeSeries(t, inner, "gpuworks_infer_prompt_tokens_total", engineLabels("i1"), start, step, ramp(ticks, 200))
	writeSeries(t, inner, "gpuworks_infer_generation_tokens_total", engineLabels("i1"), start, step, ramp(ticks, 300))
	rec := &queryRecordingStore{Store: inner}
	if _, err := BuildPanelSignals(rec, 1, panelInstances(), start.UnixMilli(), baseTime.UnixMilli(), step.Milliseconds()); err != nil {
		t.Fatal(err)
	}
	for _, call := range rec.calls {
		if len(call.metric) >= len("gpuworks_proxy_") && call.metric[:len("gpuworks_proxy_")] == "gpuworks_proxy_" {
			t.Errorf("queried gateway series %s although the engine reports every signal", call.metric)
		}
	}
	if len(rec.calls) > 5 {
		t.Errorf("%d queries, want at most the 5 engine series", len(rec.calls))
	}
}

// When the gateway is needed only its token counters and the in-flight gauge
// are read, not the whole gateway vocabulary.
func TestPanelSignalsReadOnlyTheGatewaySeriesTheyNeed(t *testing.T) {
	rec := &queryRecordingStore{Store: newTestStore(t)}
	if _, err := BuildPanelSignals(rec, 1, panelInstances(), baseTime.Add(-time.Hour).UnixMilli(), baseTime.UnixMilli(), 30_000); err != nil {
		t.Fatal(err)
	}
	proxy := map[string]bool{}
	for _, call := range rec.calls {
		if len(call.metric) >= len("gpuworks_proxy_") && call.metric[:len("gpuworks_proxy_")] == "gpuworks_proxy_" {
			proxy[call.metric] = true
		}
	}
	want := map[string]bool{metricProxyInflight: true, metricGatewayPromptTokens: true, metricGatewayCompletionTokens: true}
	if len(proxy) != len(want) {
		t.Fatalf("gateway series read = %v, want %v", proxy, want)
	}
	for m := range want {
		if !proxy[m] {
			t.Errorf("did not read %s", m)
		}
	}
}

func TestPanelSignalsPropagateStoreErrors(t *testing.T) {
	boom := errors.New("tsdb unavailable")
	for _, metric := range []string{
		"gpuworks_infer_requests_running", "gpuworks_infer_requests_waiting", "gpuworks_infer_gpu_cache_usage_percent",
		"gpuworks_infer_prompt_tokens_total", "gpuworks_infer_generation_tokens_total",
		metricProxyInflight, metricGatewayPromptTokens, metricGatewayCompletionTokens,
	} {
		store := &erroringStore{Store: newTestStore(t), failMetric: metric, err: boom}
		if _, err := BuildPanelSignals(store, 1, panelInstances(), 0, 60_000, 10_000); !errors.Is(err, boom) {
			t.Errorf("a failing %s gave %v, want the store's error", metric, err)
		}
	}
}
