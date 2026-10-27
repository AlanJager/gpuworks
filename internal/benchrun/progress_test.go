package benchrun

import (
	"context"
	"encoding/json"
	"sync/atomic"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/pkg/bench/loadgen"
)

// A point's running statuses are paced, not one per completed request: each
// carries a sample summarizing every request so far, and at hundreds of
// completions a second those samples outgrew the server's cores and memory.
// The statuses come from one reporter, so no two are built at once.
func TestPointProgressIsPacedAndSerialized(t *testing.T) {
	old := progressInterval
	progressInterval = 25 * time.Millisecond
	defer func() { progressInterval = old }()

	srv := (&fakeEndpoint{}).serve(t)
	rec := &recorder{}
	hooks := rec.hooks()
	report := hooks.Report
	var inFlight, overlapped atomic.Int32
	hooks.Report = func(st proto.BenchmarkStatus) {
		if inFlight.Add(1) > 1 {
			overlapped.Add(1)
		}
		defer inFlight.Add(-1)
		time.Sleep(time.Millisecond) // a store write takes a while; overlap must still not happen
		report(st)
	}
	started := time.Now()
	Execute(context.Background(), Run{TokenizerRef: testTokenizerRef(t),
		ID: 9, Model: "m", Client: loadgen.Client{BaseURL: srv.URL},
		Dataset: json.RawMessage(`{"kind":"random"}`), Search: json.RawMessage(`{"concurrency":[8],"isl":[8],"osl":[3],"requests_per_point_multiplier":20}`),
	}, hooks)
	elapsed := time.Since(started)

	const total = 160
	var progress []proto.BenchmarkStatus
	var final *proto.BenchmarkStatus
	for i, st := range rec.statuses {
		if st.CurrentPoint == nil || st.CurrentPoint.Warmup || st.State != "running" {
			continue
		}
		if len(st.Point) > 0 {
			final = &rec.statuses[i]
		} else if st.Completed > 0 {
			progress = append(progress, st)
		}
	}
	if final == nil || final.Completed != total {
		t.Fatalf("point result status = %+v, want %d requests completed", final, total)
	}
	if overlapped.Load() > 0 {
		t.Fatalf("%d statuses were reported while another was still being reported", overlapped.Load())
	}
	if len(progress) == 0 {
		t.Fatalf("no running status while %d requests completed over %s", total, elapsed)
	}
	if limit := int(elapsed/progressInterval) + 2; len(progress) > limit {
		t.Fatalf("%d running statuses over %s, want at most one per %s (%d)", len(progress), elapsed, progressInterval, limit)
	}
	for i, st := range progress {
		if len(st.Sample) == 0 {
			t.Fatalf("running status %d has no live sample", i)
		}
		if i > 0 && st.Completed <= progress[i-1].Completed {
			t.Fatalf("running status %d reports %d completed after %d: a paced status repeats only on progress", i, st.Completed, progress[i-1].Completed)
		}
		if st.Completed > total {
			t.Fatalf("running status %d reports %d of %d", i, st.Completed, total)
		}
	}
}
