package main

import (
	"encoding/json"
	"io"
	"os"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/benchrun"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/bench/metrics"
)

func captureBenchPoint(t *testing.T, point proto.BenchmarkPointResult) string {
	t.Helper()
	raw, err := json.Marshal(point)
	if err != nil {
		t.Fatal(err)
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	previous := os.Stdout
	os.Stdout = writer
	defer func() { os.Stdout = previous; _ = reader.Close(); _ = writer.Close() }()
	printBenchPoint(raw)
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	output, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	return string(output)
}

func benchPointWithEvidence(t *testing.T, evidence *metrics.PointEvidence) proto.BenchmarkPointResult {
	t.Helper()
	raw, err := json.Marshal(benchrun.LongTailDoc{Evidence: evidence})
	if err != nil {
		t.Fatal(err)
	}
	return proto.BenchmarkPointResult{Seq: 1, Concurrency: 2, P50TTFT: 999, P99TTFT: 999, P50TPOT: 999, P50Latency: 999, P99Latency: 999, LongTail: raw}
}

func TestPrintBenchPointUsesSupportedEvidenceTimings(t *testing.T) {
	e := &metrics.PointEvidence{Version: 1, All: metrics.RequestEvidence{
		TTFT:              metrics.MetricSummary{Count: 3, P50Ms: 10, P99Ms: 30},
		TPOT:              metrics.MetricSummary{Count: 2, P50Ms: 5, P99Ms: 7},
		LastOutputLatency: metrics.MetricSummary{Count: 3, P50Ms: 40, P99Ms: 60},
		Completion:        metrics.MetricSummary{Count: 3, P50Ms: 50, P99Ms: 70},
	}}
	output := captureBenchPoint(t, benchPointWithEvidence(t, e))
	mainLine := strings.Split(output, "\n")[0]
	for _, want := range []string{"interactivity_tok/s/user=200.00", "ttft_p50=10.0ms", "ttft_p99=30.0ms", "ttft_n=3", "tpot_p50=5.00ms", "tpot_p99=7.00ms", "tpot_n=2", "last_output_p50=40.0ms", "last_output_p99=60.0ms", "last_output_n=3", "completion_p50=50.0ms", "completion_p99=70.0ms", "completion_n=3"} {
		if !strings.Contains(mainLine, want) {
			t.Fatalf("missing %q: %s", want, mainLine)
		}
	}
	if strings.Contains(mainLine, "999") || strings.Contains(mainLine, "e2el_") {
		t.Fatalf("legacy mixed timing escaped: %s", mainLine)
	}
}

func TestPrintBenchPointDistinguishesUnavailableSamplesFromObservedZero(t *testing.T) {
	e := &metrics.PointEvidence{Version: 1, All: metrics.RequestEvidence{
		TTFT:              metrics.MetricSummary{Count: 1},
		TPOT:              metrics.MetricSummary{Count: 0, P50Ms: 5, P99Ms: 7},
		LastOutputLatency: metrics.MetricSummary{Count: 0, P50Ms: 40, P99Ms: 60},
		Completion:        metrics.MetricSummary{Count: 1, P50Ms: 50, P99Ms: 70},
	}}
	mainLine := strings.Split(captureBenchPoint(t, benchPointWithEvidence(t, e)), "\n")[0]
	for _, want := range []string{"interactivity_tok/s/user=unknown", "ttft_p50=0.0ms", "ttft_n=1", "tpot_p50=unknown", "tpot_p99=unknown", "tpot_n=0", "last_output_p50=unknown", "last_output_p99=unknown", "last_output_n=0", "completion_p50=50.0ms", "completion_n=1"} {
		if !strings.Contains(mainLine, want) {
			t.Fatalf("missing %q: %s", want, mainLine)
		}
	}
}

func TestPrintBenchPointKeepsLegacyShapeForAbsentOrFutureEvidence(t *testing.T) {
	for _, e := range []*metrics.PointEvidence{nil, {Version: 99}} {
		p := benchPointWithEvidence(t, e)
		p.P50TTFT, p.P99TTFT, p.P50TPOT, p.P50Latency, p.P99Latency = 10, 30, 5, 40, 60
		output := captureBenchPoint(t, p)
		mainLine := strings.Split(output, "\n")[0]
		want := "interactivity_tok/s/user=200.00 ttft_p50=10.0ms ttft_p99=30.0ms tpot_p50=5.00ms e2el_p50=40.0ms e2el_p99=60.0ms"
		if !strings.Contains(mainLine, want) || strings.Contains(mainLine, "ttft_n=") || !strings.Contains(output, "request/session evidence=unknown") {
			t.Fatalf("legacy output=%s", output)
		}
	}
}

func TestPrintFailedAggregatePointKeepsLedgerButMarksThroughputUnknown(t *testing.T) {
	e := &metrics.PointEvidence{Version: 1, All: metrics.RequestEvidence{Attempted: 2, Success: 2}}
	p := benchPointWithEvidence(t, e)
	p.State, p.Success = "failed", 2
	output := captureBenchPoint(t, p)
	for _, want := range []string{"state=failed", "requests=2 success=2 errors=0", "output_tok/s=unknown total_tok/s=unknown"} {
		if !strings.Contains(output, want) {
			t.Fatalf("missing %q: %s", want, output)
		}
	}
	if strings.Contains(output, "tok/s=0.00") {
		t.Fatalf("unavailable throughput is shown as zero: %s", output)
	}
}
