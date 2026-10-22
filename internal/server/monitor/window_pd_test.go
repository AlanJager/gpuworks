package monitor

import (
	"math"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// counter builds a series whose values climb by step from start.
func counter(labels map[string]string, start, step float64, points int) tsdb.Series {
	s := tsdb.Series{Labels: labels}
	for i := 0; i < points; i++ {
		s.Points = append(s.Points, tsdb.Point{TimeMS: int64(i) * 10_000, Value: start + step*float64(i)})
	}
	return s
}

func TestPDWindowReadsDecodersPrefillAndGateway(t *testing.T) {
	instances := []InstanceRef{{UUID: "d1", Role: "decode"}, {UUID: "p1", Role: "prefill"}, {UUID: "p2", Role: "prefill"}}
	decode := map[string]string{"deployment": "7", "instance": "d1"}
	prefill := map[string]string{"deployment": "7", "instance": "p1"}
	series := WindowSeries{
		Running:      []tsdb.Series{counter(decode, 2, 0, 4), counter(prefill, 9, 0, 4)},
		Waiting:      []tsdb.Series{counter(decode, 0, 0, 4), counter(prefill, 3, 0, 4), counter(map[string]string{"deployment": "7", "instance": "p2"}, 1, 0, 4)},
		PromptTokens: []tsdb.Series{counter(decode, 0, 1000, 4), counter(prefill, 0, 1000, 4)},
		ProxyRequestsTotal: []tsdb.Series{
			counter(map[string]string{"deployment": "7", "class": "2xx"}, 0, 5, 4),
		},
	}
	pd := PDSeries{
		// d1 receives 900 external + 100 local tokens per step; the prefill
		// instance's own counters must not count.
		External:     []tsdb.Series{counter(decode, 0, 900, 4), counter(prefill, 0, 5000, 4)},
		LocalCompute: []tsdb.Series{counter(decode, 0, 100, 4), counter(prefill, 0, 5000, 4)},
		Fallbacks:    []tsdb.Series{counter(map[string]string{"deployment": "7", "reason": "no_prefill"}, 0, 1, 4)},
		PrefillSum:   []tsdb.Series{counter(map[string]string{"deployment": "7"}, 0, 2000, 4)},
		PrefillCount: []tsdb.Series{counter(map[string]string{"deployment": "7"}, 0, 4, 4)},
	}
	narrowed := DecodeWindowSeries(series, instances, &pd)
	if len(narrowed.Running) != 1 || narrowed.Running[0].Labels["instance"] != "d1" || len(narrowed.PromptTokens) != 1 {
		t.Fatalf("engine series not narrowed to decoders: %+v", narrowed.Running)
	}
	if len(narrowed.ProxyRequestsTotal) != 1 {
		t.Fatal("gateway series must stay whole")
	}
	w := ComputeWindow(narrowed, WindowParams{Span: DiagnosisWindow})
	if w.RunningAvg != 2 {
		t.Fatalf("running = %v, want the decoder's 2", w.RunningAvg)
	}
	ComputePDWindow(&w, pd, false)
	if !w.PD.IsPD || !w.PD.HasEffectiveness || w.PD.Effectiveness != 0.9 {
		t.Fatalf("effectiveness = %+v", w.PD)
	}
	// 15 requests, 3 fallbacks over the window.
	if !w.PD.HasFallback || w.PD.FallbackRate != 0.2 || w.PD.Fallbacks["no_prefill"] != 3 {
		t.Fatalf("fallback = %+v (requests %v)", w.PD, w.ProxyRequestsDelta)
	}
	if !w.PD.HasPrefillStage || w.PD.PrefillStageAvgSec != 0.5 {
		t.Fatalf("prefill stage = %v", w.PD.PrefillStageAvgSec)
	}
	if !w.PD.HasPrefillWaiting || w.PD.PrefillWaitingAvg != 4 {
		t.Fatalf("prefill waiting = %v", w.PD.PrefillWaitingAvg)
	}
	if w.PD.AvgPromptTokens != 200 {
		t.Fatalf("avg prompt = %v", w.PD.AvgPromptTokens)
	}
}

func TestPDWindowWithoutTraffic(t *testing.T) {
	var w Window
	ComputePDWindow(&w, PDSeries{}, true)
	if !w.PD.IsPD || w.PD.HasEffectiveness || w.PD.HasFallback || w.PD.HasPrefillStage || w.PD.HasPrefillWaiting ||
		!w.PD.TransferExposed || w.PD.HasTransfer {
		t.Fatalf("empty window = %+v", w.PD)
	}
}

// The KV transfer comes from the decoders' NIXL histograms (the prefill
// side pulls nothing): per transfer the mean, the p95 (the max of the p95
// gauge across the window), the mean size and the rate while transferring;
// the decoders' failures and the prefill instances' expired KV are counted. A mode whose engine exports none
// (vllm-ascend's Mooncake connector) has nothing, whatever stray series
// there are.
func TestPDWindowReadsTheKVTransfer(t *testing.T) {
	instances := []InstanceRef{{UUID: "d1", Role: "decode"}, {UUID: "p1", Role: "prefill"}}
	decode := map[string]string{"deployment": "7", "instance": "d1"}
	prefill := map[string]string{"deployment": "7", "instance": "p1"}
	gauge := func(labels map[string]string, values ...float64) tsdb.Series {
		s := tsdb.Series{Labels: labels}
		for i, v := range values {
			s.Points = append(s.Points, tsdb.Point{TimeMS: int64(i) * 10_000, Value: v})
		}
		return s
	}
	with := func(labels map[string]string, k, v string) map[string]string {
		out := map[string]string{k: v}
		for key, val := range labels {
			out[key] = val
		}
		return out
	}
	build := func() PDSeries {
		return PDSeries{
			// 3 steps of 4 transfers taking 2.8 s and moving 920 MB each step.
			TransferSecondsCount: []tsdb.Series{counter(decode, 0, 4, 4), counter(prefill, 0, 100, 4)},
			TransferSecondsSum:   []tsdb.Series{counter(decode, 0, 2.8, 4), counter(prefill, 0, 100, 4)},
			TransferBytesCount:   []tsdb.Series{counter(decode, 0, 4, 4)},
			TransferBytesSum:     []tsdb.Series{counter(decode, 0, 920e6, 4)},
			TransferSeconds: []tsdb.Series{
				gauge(with(decode, "quantile", "0.95"), 0.9, 1.4, 1.1, 1.0),
				gauge(with(decode, "quantile", "0.5"), 0.6, 0.6, 0.6, 0.6),
				gauge(with(prefill, "quantile", "0.95"), 9, 9, 9, 9),
			},
			TransferFailed: []tsdb.Series{counter(decode, 0, 1, 4)},
			// vLLM counts expired KV on the prefill side only.
			TransferExpired: []tsdb.Series{counter(decode, 0, 5, 4), counter(prefill, 0, 1, 4)},
		}
	}
	pd := build()
	DecodeWindowSeries(WindowSeries{}, instances, &pd)
	var w Window
	ComputePDWindow(&w, pd, true)
	got := w.PD
	near := func(a, b float64) bool { return math.Abs(a-b) <= 1e-9*math.Max(1, math.Abs(b)) }
	if !got.HasTransfer || !near(got.TransferAvgSec, 0.7) || !got.HasTransferP95 || got.TransferP95Sec != 1.4 ||
		!near(got.TransferAvgBytes, 230e6) || !near(got.TransferBytesPerSec, 920e6/2.8) || got.TransferFailed != 3 || got.TransferExpired != 3 {
		t.Fatalf("transfer = %+v", got)
	}
	pd = build()
	DecodeWindowSeries(WindowSeries{}, instances, &pd)
	ComputePDWindow(&w, pd, false)
	if w.PD.TransferExposed || w.PD.HasTransfer || w.PD.TransferAvgSec != 0 {
		t.Fatalf("a mode without transfer metrics = %+v", w.PD)
	}
}
