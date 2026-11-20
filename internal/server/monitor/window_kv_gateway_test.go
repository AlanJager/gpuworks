package monitor

import (
	"context"
	"testing"
	"time"
)

const kvTicks = 24

func kvRamp(perTick float64) []float64 {
	out := make([]float64, kvTicks)
	for i := range out {
		out[i] = perTick * float64(i)
	}
	return out
}

func kvFlat(v float64) []float64 {
	out := make([]float64, kvTicks)
	for i := range out {
		out[i] = v
	}
	return out
}

// An engine that exports no cache metrics: the hit rate is the cached prompt
// tokens over the prompt tokens of the replies that stated a cache figure.
// The replies without the figure are in prompt_tokens_total but not in the
// denominator, so they do not read as misses. The window, the service chart
// and the fleet services row all carry it, and the source says gateway.
func TestGatewayStandsInForTheCacheHitRate(t *testing.T) {
	store := newTestStore(t)
	start := baseTime.Add(-4 * time.Minute)
	step := 10 * time.Second
	writeSeries(t, store, "gpuworks_proxy_prompt_tokens_total", gatewayLabels(nil), start, step, kvRamp(400))
	writeSeries(t, store, "gpuworks_proxy_cached_tokens_total", gatewayLabels(nil), start, step, kvRamp(30))
	writeSeries(t, store, "gpuworks_proxy_cache_reported_prompt_tokens_total", gatewayLabels(nil), start, step, kvRamp(100))
	instances := strataInventory().Instances

	series, err := FetchWindowSeries(store, 1, instances, baseTime, baseTime.Add(-DiagnosisWindow), WindowStep.Milliseconds())
	if err != nil {
		t.Fatal(err)
	}
	if series.Sources[SignalKVHitRate] != SourceGateway {
		t.Fatalf("kv_hit_rate source = %q, want gateway; sources %v", series.Sources[SignalKVHitRate], series.Sources)
	}
	w := ComputeWindow(series, WindowParams{Now: baseTime, Span: DiagnosisWindow})
	if !w.HasKVHitRate || !near(w.KVHitRate, 0.3) {
		t.Fatalf("hit rate = has %v %v, want 0.3 (30 of every 100 reported prompt tokens, not 30 of 400)", w.HasKVHitRate, w.KVHitRate)
	}

	deps := Deps{TSDB: store, Inventory: inventoryFunc(strataInventory()), Now: func() time.Time { return baseTime }}
	snap, err := BuildService(context.Background(), deps, 1, 10*time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	if snap.Sources[SignalKVHitRate] != SourceGateway {
		t.Errorf("service sources = %v", snap.Sources)
	}
	var hit []float64
	for _, s := range snap.Charts["kv"] {
		if s.Name == "hit_rate" {
			for _, p := range s.Points {
				hit = append(hit, p.Value)
			}
		}
	}
	if len(hit) == 0 {
		t.Fatalf("no hit_rate chart line from the gateway: %+v", snap.Charts["kv"])
	}
	for _, v := range hit {
		if !near(v, 0.3) {
			t.Errorf("chart hit rate %v, want 0.3 at every step", v)
		}
	}
	if !snap.Window.HasKVHitRate || !near(snap.Window.KVHitRate, 0.3) {
		t.Errorf("service window hit rate = %v %v", snap.Window.HasKVHitRate, snap.Window.KVHitRate)
	}
	// The replica rows are the engine's own per-instance view; the gateway's
	// deployment-wide figure is not attributed to a replica.
	for _, row := range snap.Replicas {
		if row.HasKVHitRate {
			t.Errorf("replica %s carries the gateway's hit rate", row.InstanceUUID)
		}
	}

	cluster, err := BuildCluster(context.Background(), deps, time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if len(cluster.Services) != 1 || !cluster.Services[0].HasKVHitRate || !near(cluster.Services[0].KVHitRate, 0.3) {
		t.Fatalf("service row = %+v", cluster.Services)
	}
}

// Without a single reply that stated a cache figure the signal is absent:
// not 0%, and no source is recorded. That covers a deployment with usage but
// no cache figure, one whose figures only arrived before the window, and one
// with no cache series at all (written before this signal existed).
func TestNoGatewayCacheHitRateWithoutReportedCacheFigures(t *testing.T) {
	start := baseTime.Add(-4 * time.Minute)
	step := 10 * time.Second
	run := func(t *testing.T, setup func(write func(metric string, values []float64))) {
		t.Helper()
		store := newTestStore(t)
		setup(func(metric string, values []float64) {
			writeSeries(t, store, metric, gatewayLabels(nil), start, step, values)
		})
		series, err := FetchWindowSeries(store, 1, strataInventory().Instances, baseTime, baseTime.Add(-DiagnosisWindow), WindowStep.Milliseconds())
		if err != nil {
			t.Fatal(err)
		}
		if source, ok := series.Sources[SignalKVHitRate]; ok {
			t.Errorf("kv_hit_rate has source %q with nothing reported", source)
		}
		w := ComputeWindow(series, WindowParams{Now: baseTime, Span: DiagnosisWindow})
		if w.HasKVHitRate {
			t.Errorf("hit rate present as %v, want absent", w.KVHitRate)
		}
		snap, err := BuildService(context.Background(), Deps{TSDB: store, Inventory: inventoryFunc(strataInventory()), Now: func() time.Time { return baseTime }}, 1, 10*time.Minute)
		if err != nil {
			t.Fatal(err)
		}
		for _, s := range snap.Charts["kv"] {
			if s.Name == "hit_rate" && len(s.Points) > 0 {
				t.Errorf("a hit_rate chart line was drawn from nothing: %+v", s)
			}
		}
	}
	t.Run("usage without any cache figure", func(t *testing.T) {
		run(t, func(write func(string, []float64)) {
			write("gpuworks_proxy_prompt_tokens_total", kvRamp(400))
			write("gpuworks_proxy_cached_tokens_total", kvFlat(0))
			write("gpuworks_proxy_cache_reported_prompt_tokens_total", kvFlat(0))
		})
	})
	t.Run("figures only before the window", func(t *testing.T) {
		run(t, func(write func(string, []float64)) {
			write("gpuworks_proxy_cached_tokens_total", kvFlat(500))
			write("gpuworks_proxy_cache_reported_prompt_tokens_total", kvFlat(1000))
		})
	})
	t.Run("cached tokens without a denominator", func(t *testing.T) {
		run(t, func(write func(string, []float64)) {
			write("gpuworks_proxy_prompt_tokens_total", kvRamp(400))
			write("gpuworks_proxy_cached_tokens_total", kvRamp(30))
		})
	})
}

// The engine's own hit rate wins over the gateway's, whether it reports the
// vLLM counters or the SGLang instant gauge.
func TestEngineCacheHitRateWinsOverTheGateway(t *testing.T) {
	start := baseTime.Add(-4 * time.Minute)
	step := 10 * time.Second
	engineLabels := map[string]string{"instance": "s1", "deployment": "1"}
	cases := []struct {
		name  string
		write func(t *testing.T, put func(metric string, labels map[string]string, values []float64))
		want  float64
	}{
		{"vllm counters", func(t *testing.T, put func(string, map[string]string, []float64)) {
			put("gpuworks_infer_kv_queries_total", engineLabels, kvRamp(1000))
			put("gpuworks_infer_kv_hits_total", engineLabels, kvRamp(800))
		}, 0.8},
		{"sglang instant gauge", func(t *testing.T, put func(string, map[string]string, []float64)) {
			put("gpuworks_infer_kv_hit_rate_instant", engineLabels, kvFlat(0.55))
		}, 0.55},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			store := newTestStore(t)
			put := func(metric string, labels map[string]string, values []float64) {
				writeSeries(t, store, metric, labels, start, step, values)
			}
			put("gpuworks_proxy_cached_tokens_total", gatewayLabels(nil), kvRamp(30))
			put("gpuworks_proxy_cache_reported_prompt_tokens_total", gatewayLabels(nil), kvRamp(100))
			tc.write(t, put)
			series, err := FetchWindowSeries(store, 1, strataInventory().Instances, baseTime, baseTime.Add(-DiagnosisWindow), WindowStep.Milliseconds())
			if err != nil {
				t.Fatal(err)
			}
			if series.Sources[SignalKVHitRate] != SourceEngine {
				t.Fatalf("source = %q, want engine", series.Sources[SignalKVHitRate])
			}
			w := ComputeWindow(series, WindowParams{Now: baseTime, Span: DiagnosisWindow})
			if !w.HasKVHitRate || !near(w.KVHitRate, tc.want) {
				t.Fatalf("hit rate = has %v %v, want the engine's %v", w.HasKVHitRate, w.KVHitRate, tc.want)
			}
		})
	}
}
