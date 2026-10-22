package metrics

import (
	"errors"
	"math"
	"testing"
	"time"
)

func TestAccumulatorPercentilesAndDerivedMetrics(t *testing.T) {
	a := New(time.Second)
	start := time.Now().Add(-100 * time.Millisecond)
	a.Add(Request{StartedAt: start, TTFT: 10 * time.Millisecond, TPOT: 20 * time.Millisecond, Latency: 30 * time.Millisecond, InputTokens: 10, OutputTokens: 3, CachedTokens: 5, CachedTokensAvailable: true, Chunks: 3})
	a.Add(Request{StartedAt: start.Add(10 * time.Millisecond), TTFT: 20 * time.Millisecond, TPOT: 40 * time.Millisecond, Latency: 50 * time.Millisecond, InputTokens: 20, OutputTokens: 5, CachedTokens: 10, CachedTokensAvailable: true, Chunks: 5})
	s := a.Snapshot(time.Now())
	if s.Success != 2 || s.Errors != 0 || s.TTFT[50] != 10*time.Millisecond || s.InputTokens != 30 || s.OutputTokens != 8 {
		t.Fatalf("unexpected snapshot: %+v", s)
	}
	if s.KVHitRate == nil || *s.KVHitRate != 0.5 {
		t.Fatalf("kv hit rate = %v", s.KVHitRate)
	}
	if s.CacheReportCoverage == nil || *s.CacheReportCoverage != 1 || s.PeakInputTokens != 20 {
		t.Fatalf("cache coverage/peak input = %v/%d", s.CacheReportCoverage, s.PeakInputTokens)
	}
	if s.Speculative == nil || s.Speculative.TokensPerIteration != 1 {
		t.Fatalf("speculative = %+v", s.Speculative)
	}
}

func TestProcessSampleIncludesOutcomeCounts(t *testing.T) {
	start := time.Now().Add(-time.Second)
	a := New(10 * time.Second)
	a.Add(Request{StartedAt: start, Latency: time.Second, OutputTokens: 10})
	a.Add(Request{StartedAt: start, Latency: time.Second, Error: errors.New("boom")})
	sample := a.Snapshot(time.Now()).ProcessSample()
	if sample.Success != 1 || sample.Errors != 1 {
		t.Fatalf("sample counts = %d/%d, want 1/1", sample.Success, sample.Errors)
	}
}

func TestRollingPeakShortPointEqualsAverage(t *testing.T) {
	start := time.Unix(100, 0)
	requests := []Request{{StartedAt: start, TTFT: time.Second, Latency: 5 * time.Second, InputTokens: 50, OutputTokens: 100}}
	peak := RollingPeakThroughput(requests, start.Add(5*time.Second), 10*time.Second)
	closeFloat(t, peak.Output, 20)
	closeFloat(t, peak.Total, 30)
}

func TestRollingPeakSelectsBusyWindowAndConcurrentWork(t *testing.T) {
	start := time.Unix(100, 0)
	requests := []Request{
		{StartedAt: start, Latency: 20 * time.Second, OutputTokens: 100},
		{StartedAt: start.Add(10 * time.Second), Latency: 10 * time.Second, OutputTokens: 200},
		{StartedAt: start.Add(10 * time.Second), Latency: 10 * time.Second, OutputTokens: 100},
	}
	peak := RollingPeakThroughput(requests, start.Add(20*time.Second), 10*time.Second)
	closeFloat(t, peak.Output, 35)
	closeFloat(t, peak.Total, 35)
}

func TestRollingPeakProratesLongRequestAtWindowBoundary(t *testing.T) {
	start := time.Unix(100, 0)
	requests := []Request{{StartedAt: start, Latency: 20 * time.Second, OutputTokens: 200}}
	peak := RollingPeakThroughput(requests, start.Add(20*time.Second), 10*time.Second)
	closeFloat(t, peak.Output, 10)
}

func TestRollingPeakSeparatesPrefillAndDecodeAndExcludesErrors(t *testing.T) {
	start := time.Unix(100, 0)
	requests := []Request{
		{StartedAt: start, TTFT: 5 * time.Second, Latency: 10 * time.Second, InputTokens: 100, OutputTokens: 50},
		{StartedAt: start, TTFT: 5 * time.Second, Latency: 10 * time.Second, InputTokens: 1000, OutputTokens: 1000, Error: errors.New("failed")},
	}
	peak := RollingPeakThroughput(requests, start.Add(10*time.Second), 5*time.Second)
	closeFloat(t, peak.Output, 10)
	closeFloat(t, peak.Total, 20)
}

func closeFloat(t *testing.T, got, want float64) {
	t.Helper()
	if math.Abs(got-want) > 1e-9 {
		t.Fatalf("got %v, want %v", got, want)
	}
}

func TestAccumulatorKVRateUnknownWithoutCachedTokens(t *testing.T) {
	a := New(time.Second)
	a.Add(Request{StartedAt: time.Now(), InputTokens: 10, OutputTokens: 2, Latency: time.Millisecond})
	if got := a.Snapshot(time.Now()).KVHitRate; got != nil {
		t.Fatalf("expected unknown KV hit rate, got %v", *got)
	}
	coverage := a.Snapshot(time.Now()).CacheReportCoverage
	if coverage == nil || *coverage != 0 {
		t.Fatalf("expected zero cache-report coverage, got %v", coverage)
	}
}

func TestAccumulatorMarksApproximateTokens(t *testing.T) {
	a := New(time.Second)
	a.Add(Request{StartedAt: time.Now(), InputTokens: 10, OutputTokens: 2, ApproximateTokens: true, Latency: time.Millisecond})
	if !a.Snapshot(time.Now()).ApproximateTokens {
		t.Fatal("expected approximate token marker")
	}
}

func TestAccumulatorUsageTokensRemainExact(t *testing.T) {
	a := New(time.Second)
	a.Add(Request{StartedAt: time.Now(), InputTokens: 10, OutputTokens: 2, Latency: time.Millisecond})
	if a.Snapshot(time.Now()).ApproximateTokens {
		t.Fatal("usage-backed request must remain exact")
	}
}

func TestAccumulatorWindowAndErrors(t *testing.T) {
	a := New(time.Second)
	now := time.Now()
	a.Add(Request{StartedAt: now.Add(-2 * time.Second), Error: errTest})
	a.Add(Request{StartedAt: now, Latency: time.Millisecond, OutputTokens: 1})
	if got := a.WindowSnapshot(now); got.Count != 1 || got.Success != 1 {
		t.Fatalf("window snapshot = %+v", got)
	}
}

var errTest = testError("boom")

type testError string

func (e testError) Error() string { return string(e) }

func TestTPOTExcludesResponsesWithoutDecodeInterval(t *testing.T) {
	start := time.Now().Add(-time.Second)
	a := New(10 * time.Second)
	// A streamed request: 2 s TTFT, 9 tokens over 80 ms of decode = 10 ms/token.
	a.Add(Request{StartedAt: start, TTFT: 2 * time.Second, TPOT: 80 * time.Millisecond, Latency: 2080 * time.Millisecond, OutputTokens: 9, Chunks: 9})
	// A response delivered in one chunk has several tokens but no observable
	// decode interval (loadgen leaves TPOT unset); it must not count as 0 ms.
	a.Add(Request{StartedAt: start, TTFT: 300 * time.Millisecond, Latency: 300 * time.Millisecond, OutputTokens: 40, Chunks: 1})
	// A one-token answer has no inter-token gap either.
	a.Add(Request{StartedAt: start, TTFT: 100 * time.Millisecond, Latency: 100 * time.Millisecond, OutputTokens: 1, Chunks: 1})
	s := a.Snapshot(time.Now())
	for _, p := range []int{1, 50, 99} {
		if s.TPOT[p] != 10*time.Millisecond {
			t.Fatalf("TPOT P%d = %v, want 10ms from the only streamed request", p, s.TPOT[p])
		}
	}
	avg := AverageRequestMetrics(a.Requests())
	if avg.TPOT != 10 {
		t.Fatalf("average TPOT = %v, want 10 (requests without TPOT must not dilute it)", avg.TPOT)
	}
	if avg.TTFT != 800 || math.Abs(avg.Latency-826.6666) > 0.001 {
		t.Fatalf("averages = %+v", avg)
	}
	if empty := AverageRequestMetrics([]Request{{Error: errors.New("failed")}}); empty != (RequestAverages{}) {
		t.Fatalf("failed-only averages = %+v", empty)
	}
}

func TestInteractivityIsInverseTPOT(t *testing.T) {
	for _, tc := range []struct{ tpot, want float64 }{{20, 50}, {4, 250}, {0, 0}, {-1, 0}, {math.NaN(), 0}, {math.Inf(1), 0}} {
		if got := Interactivity(tc.tpot); got != tc.want {
			t.Fatalf("Interactivity(%v) = %v, want %v", tc.tpot, got, tc.want)
		}
	}
}

func TestAverageTokenLengthsPerSuccessfulRequest(t *testing.T) {
	in, out := Snapshot{Success: 2, InputTokens: 20, OutputTokens: 8}.AverageTokenLengths()
	if in != 10 || out != 4 {
		t.Fatalf("lengths = %v/%v", in, out)
	}
	if in, out := (Snapshot{}).AverageTokenLengths(); in != 0 || out != 0 {
		t.Fatalf("empty lengths = %v/%v", in, out)
	}
}
