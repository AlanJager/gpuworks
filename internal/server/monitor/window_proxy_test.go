package monitor

import (
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/tsdb"
)

// TestComputeWindowProxySignals writes proxy accounting counters directly
// (the phase2 metrics contract's gpuworks_proxy_requests_total{deployment,
// class} etc., emitted server-side with no per-instance concept) and checks
// the derived request/error rates.
func TestComputeWindowProxySignals(t *testing.T) {
	store := newTestStore(t)
	dep := map[string]string{"deployment": "60"}
	classLabels := func(class string) map[string]string {
		return map[string]string{"deployment": "60", "class": class}
	}

	start := baseTime.Add(-70 * time.Second)
	for i := 0; i < 8; i++ {
		at := start.Add(time.Duration(i) * 10 * time.Second)
		// Cumulative counters: 100 total requests/tick growth, 90 2xx, 5
		// 5xx, 3 429, 2 asleep_503 (ratios 5%, 3%, 2%).
		writeSample(t, store, "gpuworks_proxy_requests_total", classLabels("2xx"), float64(90*(i+1)), at)
		writeSample(t, store, "gpuworks_proxy_requests_total", classLabels("5xx"), float64(5*(i+1)), at)
		writeSample(t, store, "gpuworks_proxy_requests_total", classLabels("429"), float64(3*(i+1)), at)
		writeSample(t, store, "gpuworks_proxy_requests_total", classLabels("asleep_503"), float64(2*(i+1)), at)
		writeSample(t, store, "gpuworks_proxy_requests_inflight", dep, 4, at)
	}

	w := computeWindowFromStore(t, store, 60, nil, baseTime, 5*time.Minute)

	if !w.HasRequestRate || w.RequestsPerSec != 10 {
		t.Errorf("RequestsPerSec = %v has=%v, want 10/true (100 total per 10s tick)", w.RequestsPerSec, w.HasRequestRate)
	}
	if !w.HasErrRate {
		t.Fatal("expected HasErrRate")
	}
	if got, want := w.ErrRate5xx, 0.05; got < want-1e-9 || got > want+1e-9 {
		t.Errorf("ErrRate5xx = %v, want %v", got, want)
	}
	if got, want := w.ErrRate429, 0.03; got < want-1e-9 || got > want+1e-9 {
		t.Errorf("ErrRate429 = %v, want %v", got, want)
	}
	if got, want := w.ErrRateAsleep, 0.02; got < want-1e-9 || got > want+1e-9 {
		t.Errorf("ErrRateAsleep = %v, want %v", got, want)
	}
	if !w.HasInflight || w.InflightAvg != 4 {
		t.Errorf("InflightAvg = %v has=%v, want 4/true", w.InflightAvg, w.HasInflight)
	}
}

func TestComputeWindowProxySignalsAbsentLeavesHasFlagsFalse(t *testing.T) {
	store := newTestStore(t)
	w := computeWindowFromStore(t, store, 61, nil, baseTime, 5*time.Minute)
	if w.HasRequestRate || w.HasErrRate || w.HasInflight {
		t.Fatalf("expected every proxy Has* flag false with no data, got %+v", w)
	}
}

func TestRatioSeriesSkipsMissingOrNonPositiveDenominatorBuckets(t *testing.T) {
	num := tsdb.Series{Points: []tsdb.Point{{TimeMS: 1000, Value: 1}, {TimeMS: 2000, Value: 2}, {TimeMS: 3000, Value: 3}}}
	denom := tsdb.Series{Points: []tsdb.Point{{TimeMS: 1000, Value: 10}, {TimeMS: 3000, Value: 0}}}
	out := ratioSeries(num, denom)
	if len(out.Points) != 1 {
		t.Fatalf("ratioSeries points = %+v, want exactly 1 (bucket 2000 missing from denom, bucket 3000's denom <= 0)", out.Points)
	}
	if out.Points[0].TimeMS != 1000 || out.Points[0].Value != 0.1 {
		t.Errorf("ratioSeries = %+v, want {1000, 0.1}", out.Points[0])
	}
}

// TestComputeWindowProxyErrRate4xx proves ErrRate4xx is populated from the
// "4xx" class share.
func TestComputeWindowProxyErrRate4xx(t *testing.T) {
	store := newTestStore(t)
	classLabels := func(class string) map[string]string {
		return map[string]string{"deployment": "63", "class": class}
	}
	start := baseTime.Add(-70 * time.Second)
	for i := 0; i < 8; i++ {
		at := start.Add(time.Duration(i) * 10 * time.Second)
		writeSample(t, store, "gpuworks_proxy_requests_total", classLabels("2xx"), float64(80*(i+1)), at)
		writeSample(t, store, "gpuworks_proxy_requests_total", classLabels("4xx"), float64(20*(i+1)), at) // 20%
	}
	w := computeWindowFromStore(t, store, 63, nil, baseTime, 5*time.Minute)
	if !w.HasErrRate {
		t.Fatal("expected HasErrRate")
	}
	if got, want := w.ErrRate4xx, 0.2; got < want-1e-9 || got > want+1e-9 {
		t.Errorf("ErrRate4xx = %v, want %v", got, want)
	}
}

// TestComputeWindowProxyRequestsDeltaSumsAcrossClasses proves
// ProxyRequestsDelta/ClientErrorsDelta/ServerErrorsDelta are the counters'
// actual accumulated growth over the window (first vs last sample per
// class), not RequestsPerSec/ErrRate*'s rate-then-multiply-by-span
// approximation - the alignment row (monitorapi.buildAlignment) needs the
// exact figure so it lines up with the engine's own success-counter delta.
func TestComputeWindowProxyRequestsDeltaSumsAcrossClasses(t *testing.T) {
	store := newTestStore(t)
	classLabels := func(class string) map[string]string {
		return map[string]string{"deployment": "65", "class": class}
	}
	start := baseTime.Add(-70 * time.Second)
	for i := 0; i < 8; i++ {
		at := start.Add(time.Duration(i) * 10 * time.Second)
		n := float64(i + 1)
		// Per tick: 70 2xx, 10 4xx, 5 429, 10 5xx, 5 asleep_503 (100 total);
		// over 8 ticks (n=1..8) each class grows by class_per_tick*(8-1).
		writeSample(t, store, "gpuworks_proxy_requests_total", classLabels("2xx"), 70*n, at)
		writeSample(t, store, "gpuworks_proxy_requests_total", classLabels("4xx"), 10*n, at)
		writeSample(t, store, "gpuworks_proxy_requests_total", classLabels("429"), 5*n, at)
		writeSample(t, store, "gpuworks_proxy_requests_total", classLabels("5xx"), 10*n, at)
		writeSample(t, store, "gpuworks_proxy_requests_total", classLabels("asleep_503"), 5*n, at)
	}

	w := computeWindowFromStore(t, store, 65, nil, baseTime, 5*time.Minute)

	// Each class grows by class_per_tick*(n_last - n_first) = class_per_tick*7.
	wantProxy := (70 + 10 + 5 + 10 + 5) * 7.0
	wantClient := (10 + 5) * 7.0
	wantServer := (10 + 5) * 7.0
	if got := w.ProxyRequestsDelta; got != wantProxy {
		t.Errorf("ProxyRequestsDelta = %v, want %v", got, wantProxy)
	}
	if got := w.ClientErrorsDelta; got != wantClient {
		t.Errorf("ClientErrorsDelta = %v, want %v", got, wantClient)
	}
	if got := w.ServerErrorsDelta; got != wantServer {
		t.Errorf("ServerErrorsDelta = %v, want %v", got, wantServer)
	}
}

// TestComputeWindowProxyRequestsDeltaResetAware proves ProxyRequestsDelta
// walks every consecutive pair of samples (the same reset handling as
// SumSeriesDelta/Rate) rather than naive last-minus-first math, which would
// under-count after a mid-window counter reset (proxy restart).
func TestComputeWindowProxyRequestsDeltaResetAware(t *testing.T) {
	store := newTestStore(t)
	labels := map[string]string{"deployment": "66", "class": "2xx"}
	// Counter climbs to 1000, resets (proxy restarted), climbs to 60, then
	// to 1100. Naive first-vs-last would read 1100-1000=100; walking every
	// step with reset handling gives 1000 (0->1000) + 60 (reset, treated as
	// the new post-reset value) + 1040 (60->1100) = 2100.
	values := []float64{0, 1000, 60, 1100}
	for i, v := range values {
		at := baseTime.Add(time.Duration(i) * 10 * time.Second)
		writeSample(t, store, "gpuworks_proxy_requests_total", labels, v, at)
	}

	w := computeWindowFromStore(t, store, 66, nil, baseTime.Add(30*time.Second), 5*time.Minute)
	want := 1000.0 + 60.0 + 1040.0
	if got := w.ProxyRequestsDelta; got != want {
		t.Errorf("ProxyRequestsDelta = %v, want %v (reset-aware, not naive first-vs-last)", got, want)
	}
}

// TestComputeWindowInflightLastAndMax proves InflightLast is the gauge's own
// most recent sample (not InflightAvg's window-wide average) and InflightMax
// is the largest value seen anywhere in the window - the two figures
// monitorapi's alignment row needs (InFlight, and DiscrepancyPersistent's
// "zero for the whole window" check).
func TestComputeWindowInflightLastAndMax(t *testing.T) {
	store := newTestStore(t)
	dep := map[string]string{"deployment": "67"}
	values := []float64{0, 8, 3, 5}
	for i, v := range values {
		at := baseTime.Add(time.Duration(i) * 10 * time.Second)
		writeSample(t, store, "gpuworks_proxy_requests_inflight", dep, v, at)
	}

	w := computeWindowFromStore(t, store, 67, nil, baseTime.Add(30*time.Second), 5*time.Minute)
	if !w.HasInflight {
		t.Fatal("expected HasInflight")
	}
	if w.InflightLast != 5 {
		t.Errorf("InflightLast = %v, want 5 (the last sample)", w.InflightLast)
	}
	if w.InflightMax != 8 {
		t.Errorf("InflightMax = %v, want 8 (the largest sample in the window)", w.InflightMax)
	}
	if w.InflightAvg != 4 {
		t.Errorf("InflightAvg = %v, want 4 (0+8+3+5)/4", w.InflightAvg)
	}
}

// TestComputeWindowInflightZeroWholeWindow proves InflightMax reads exactly
// 0 (not just InflightLast) when every sample in the window was 0 - the
// case DiscrepancyPersistent depends on to tell "genuinely lost" apart from
// "some request was in flight at some point, even if not at the very end".
func TestComputeWindowInflightZeroWholeWindow(t *testing.T) {
	store := newTestStore(t)
	dep := map[string]string{"deployment": "68"}
	for i := 0; i < 4; i++ {
		at := baseTime.Add(time.Duration(i) * 10 * time.Second)
		writeSample(t, store, "gpuworks_proxy_requests_inflight", dep, 0, at)
	}
	w := computeWindowFromStore(t, store, 68, nil, baseTime.Add(30*time.Second), 5*time.Minute)
	if !w.HasInflight {
		t.Fatal("expected HasInflight")
	}
	if w.InflightMax != 0 || w.InflightLast != 0 {
		t.Errorf("InflightMax/InflightLast = %v/%v, want 0/0", w.InflightMax, w.InflightLast)
	}
}
