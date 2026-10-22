package loadgen

import (
	"context"
	"math"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func gapStats(gaps []time.Duration) (mean, cv float64) {
	var sum, sq float64
	for _, g := range gaps {
		sum += g.Seconds()
	}
	mean = sum / float64(len(gaps))
	for _, g := range gaps {
		d := g.Seconds() - mean
		sq += d * d
	}
	return mean, math.Sqrt(sq/float64(len(gaps))) / mean
}

func TestArrivalGapsFollowRateAndBurstiness(t *testing.T) {
	// Gamma(shape k) has coefficient of variation 1/sqrt(k): Poisson is 1,
	// bursty traffic above 1, smoothed traffic below 1. The mean stays 1/rate.
	for _, tc := range []struct{ burstiness, cv float64 }{{1, 1}, {0.25, 2}, {4, 0.5}} {
		gaps := Arrival{Rate: 4, Burstiness: tc.burstiness, Seed: 42}.Gaps(40001)
		if len(gaps) != 40000 {
			t.Fatalf("gaps = %d", len(gaps))
		}
		mean, cv := gapStats(gaps)
		if math.Abs(mean-0.25)/0.25 > 0.03 {
			t.Fatalf("burstiness %v: mean gap = %v, want 0.25s", tc.burstiness, mean)
		}
		if math.Abs(cv-tc.cv)/tc.cv > 0.05 {
			t.Fatalf("burstiness %v: cv = %v, want %v", tc.burstiness, cv, tc.cv)
		}
	}
	a := Arrival{Rate: 2, Seed: 7}.Gaps(10)
	b := Arrival{Rate: 2, Seed: 7}.Gaps(10)
	for i := range a {
		if a[i] != b[i] {
			t.Fatal("a fixed seed must reproduce the schedule")
		}
	}
	if (Arrival{Rate: 2}).Gaps(1) != nil || (Arrival{}).Gaps(5) != nil {
		t.Fatal("no gaps for a single arrival or a zero rate")
	}
}

// blockingServer answers a streamed completion only after release is closed,
// counting requests in flight.
func blockingServer(t *testing.T, release <-chan struct{}) (*httptest.Server, *atomic.Int32, *atomic.Int32) {
	t.Helper()
	var inFlight, peak atomic.Int32
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		n := inFlight.Add(1)
		for {
			p := peak.Load()
			if n <= p || peak.CompareAndSwap(p, n) {
				break
			}
		}
		defer inFlight.Add(-1)
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "text/event-stream")
		_, _ = w.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"a\"}}]}\n\ndata: {\"choices\":[{\"delta\":{\"content\":\"b\"}}]}\n\n"))
		_, _ = w.Write([]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":3,\"completion_tokens\":2}}\n\ndata: [DONE]\n\n"))
	}))
	t.Cleanup(srv.Close)
	return srv, &inFlight, &peak
}

func TestOpenLoopDispatchesOnScheduleRegardlessOfInFlight(t *testing.T) {
	release := make(chan struct{})
	srv, inFlight, _ := blockingServer(t, release)
	var mu sync.Mutex
	var waits []time.Duration
	sleep := func(ctx context.Context, d time.Duration) error {
		mu.Lock()
		waits = append(waits, d)
		mu.Unlock()
		return ctx.Err()
	}
	done := make(chan []Result, 1)
	r := Runner{Client: Client{BaseURL: srv.URL}, Concurrency: 1, Arrival: &Arrival{Rate: 10, Seed: 3}, Sleep: sleep}
	go func() {
		done <- r.Run(context.Background(), []Request{{Stream: true}, {Stream: true}, {Stream: true}, {Stream: true}})
	}()
	// Concurrency 1 would hold the others back; open loop sends all four
	// while none has finished.
	deadline := time.Now().Add(2 * time.Second)
	for inFlight.Load() < 4 {
		if time.Now().After(deadline) {
			t.Fatalf("in flight = %d, want 4 before any completes", inFlight.Load())
		}
		time.Sleep(5 * time.Millisecond)
	}
	close(release)
	results := <-done
	if len(results) != 4 {
		t.Fatalf("results = %d", len(results))
	}
	want := Arrival{Rate: 10, Seed: 3}.Gaps(4)
	if len(waits) != 3 || waits[0] != want[0] || waits[2] != want[2] {
		t.Fatalf("waits = %v, want the arrival gaps %v", waits, want)
	}
}

func TestOpenLoopMaxInFlightCapsSessions(t *testing.T) {
	release := make(chan struct{})
	srv, inFlight, peak := blockingServer(t, release)
	done := make(chan []Result, 1)
	r := Runner{Client: Client{BaseURL: srv.URL}, Arrival: &Arrival{Rate: 1000, Seed: 1}, MaxInFlight: 2}
	go func() { done <- r.Run(context.Background(), make([]Request, 6)) }()
	deadline := time.Now().Add(2 * time.Second)
	for inFlight.Load() < 2 {
		if time.Now().After(deadline) {
			t.Fatal("capped sessions did not start")
		}
		time.Sleep(5 * time.Millisecond)
	}
	time.Sleep(50 * time.Millisecond)
	if p := peak.Load(); p != 2 {
		t.Fatalf("peak in flight = %d, want the cap 2", p)
	}
	close(release)
	if results := <-done; len(results) != 6 {
		t.Fatalf("results = %d", len(results))
	}
}

func TestOpenLoopCancellationStopsArrivals(t *testing.T) {
	release := make(chan struct{})
	defer close(release)
	srv, _, _ := blockingServer(t, release)
	ctx, cancel := context.WithCancel(context.Background())
	var calls atomic.Int32
	sleep := func(ctx context.Context, d time.Duration) error {
		if calls.Add(1) == 2 {
			cancel()
		}
		return ctx.Err()
	}
	results := Runner{Client: Client{BaseURL: srv.URL}, Arrival: &Arrival{Rate: 5, Seed: 2}, Sleep: sleep}.Run(ctx, make([]Request, 10))
	// Only the two arrivals before the cancel were dispatched; whichever of
	// them reached the endpoint ends canceled, and no later arrival is sent.
	if len(results) > 2 || calls.Load() != 2 {
		t.Fatalf("results = %d sleeps = %d, want at most the 2 dispatched before cancellation", len(results), calls.Load())
	}
	for _, r := range results {
		if r.Metrics.Error == nil {
			t.Fatalf("canceled session reported success: %+v", r)
		}
	}
}
