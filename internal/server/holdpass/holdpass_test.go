package holdpass

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

var quiet = slog.New(slog.NewTextHandler(io.Discard, nil))

// recorder is a pass that records when it ran and fails while failing is set.
type recorder struct {
	mu      sync.Mutex
	at      []time.Time
	failing atomic.Bool
	started chan struct{}
}

func newRecorder() *recorder { return &recorder{started: make(chan struct{}, 1000)} }

func (r *recorder) apply() ([]string, []string, error) {
	fail := r.failing.Load()
	r.mu.Lock()
	r.at = append(r.at, time.Now())
	r.mu.Unlock()
	r.started <- struct{}{}
	if fail {
		return nil, nil, errors.New("database is locked")
	}
	return []string{"n1"}, nil, nil
}

func (r *recorder) passes() []time.Time {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]time.Time(nil), r.at...)
}

func (r *recorder) waitPass(t *testing.T, within time.Duration) {
	t.Helper()
	select {
	case <-r.started:
	case <-time.After(within):
		t.Fatalf("no pass within %s", within)
	}
}

func start(t *testing.T, r *Runner) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { r.Run(ctx); close(done) }()
	return func() {
		cancel()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatal("Run did not return after its context ended")
		}
	}
}

// A request on a quiet runner is served at once; a burst of requests after
// it makes exactly one more pass, a floor later.
func TestABurstOfRequestsMakesOneMorePass(t *testing.T) {
	rec := newRecorder()
	const floor = 150 * time.Millisecond
	r := New(rec.apply, quiet, floor)
	stop := start(t, r)
	defer stop()

	begin := time.Now()
	r.Request()
	rec.waitPass(t, time.Second)
	if waited := time.Since(begin); waited > floor {
		t.Fatalf("the first pass waited %s; a quiet runner serves a request at once", waited)
	}
	for range 1000 {
		r.Request()
	}
	rec.waitPass(t, time.Second)
	time.Sleep(3 * floor)
	at := rec.passes()
	if len(at) != 2 {
		t.Fatalf("passes = %d, want 2: a burst joins one pending pass", len(at))
	}
	if gap := at[1].Sub(at[0]); gap < floor {
		t.Fatalf("passes %s apart, want at least the floor %s", gap, floor)
	}
}

// Requests spread over a period make at most one pass per floor.
func TestRequestsMakeAtMostOnePassPerFloor(t *testing.T) {
	rec := newRecorder()
	const floor = 100 * time.Millisecond
	r := New(rec.apply, quiet, floor)
	stop := start(t, r)
	begin := time.Now()
	for time.Since(begin) < 10*floor {
		r.Request()
		time.Sleep(time.Millisecond)
	}
	time.Sleep(2 * floor)
	stop()
	at := rec.passes()
	if limit := 13; len(at) > limit || len(at) < 5 {
		t.Fatalf("passes over %s of requests every millisecond = %d, want 5..%d", 12*floor, len(at), limit)
	}
	for i := 1; i < len(at); i++ {
		if gap := at[i].Sub(at[i-1]); gap < floor {
			t.Fatalf("passes %d and %d are %s apart, under the floor", i-1, i, gap)
		}
	}
}

// A failed pass is retried without another request, after the floor, then
// after twice as long; a success ends the retries.
func TestAFailedPassIsRetriedWithBackoff(t *testing.T) {
	rec := newRecorder()
	rec.failing.Store(true)
	const floor = 80 * time.Millisecond
	r := New(rec.apply, quiet, floor)
	stop := start(t, r)
	defer stop()
	r.Request()
	for range 3 {
		rec.waitPass(t, time.Second)
	}
	rec.failing.Store(false)
	rec.waitPass(t, 2*time.Second)
	time.Sleep(10 * floor)
	at := rec.passes()
	if len(at) != 4 {
		t.Fatalf("passes = %d, want 3 failures and the success that ends the retries", len(at))
	}
	if first, second := at[1].Sub(at[0]), at[2].Sub(at[1]); first < floor || second < 2*floor {
		t.Fatalf("retries after %s and %s, want >= %s and >= %s", first, second, floor, 2*floor)
	}
}

// A request made while Run is stopped, or owed when it stopped, is served
// by the next Run.
func TestAPendingRequestOutlivesAStop(t *testing.T) {
	rec := newRecorder()
	r := New(rec.apply, quiet, time.Hour)
	r.Request()
	stop := start(t, r)
	rec.waitPass(t, time.Second)
	r.Request() // owed, but the floor is an hour away
	stop()
	if n := len(rec.passes()); n != 1 {
		t.Fatalf("passes before the restart = %d", n)
	}
	stop = start(t, r)
	defer stop()
	rec.waitPass(t, time.Second)
}

// Request never blocks, with or without a Run.
func TestRequestNeverBlocks(t *testing.T) {
	r := New(func() ([]string, []string, error) { return nil, nil, nil }, nil, time.Hour)
	done := make(chan struct{})
	go func() {
		for range 10000 {
			r.Request()
		}
		close(done)
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatal("Request blocked without a Run")
	}
	if n := testing.AllocsPerRun(100, r.Request); n != 0 {
		t.Fatalf("Request allocates %v times", n)
	}
}
