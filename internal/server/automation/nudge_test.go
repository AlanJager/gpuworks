package automation

import (
	"context"
	"sync/atomic"
	"testing"
	"time"
)

// countingWorld counts the evaluator's reads; it holds nothing, so no order
// is ever given.
type countingWorld struct {
	*fakeWorld
	schedules, waits atomic.Int32
}

func (w *countingWorld) Schedules() ([]Schedule, error)       { w.schedules.Add(1); return nil, nil }
func (w *countingWorld) WaitingWakes() ([]WaitingWake, error) { w.waits.Add(1); return nil, nil }

// A burst of changes - a fleet redeploying nudges once per instance write -
// is one look at the waiting wakes, and does not reread schedules, which only
// the tick makes due.
func TestABurstOfNudgesIsOneLookAtTheWaitingWakes(t *testing.T) {
	w := &countingWorld{fakeWorld: newFakeWorld()}
	e := New(w, w, nil)
	e.interval = time.Hour
	e.settle = 200 * time.Millisecond
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { e.Run(ctx); close(done) }()

	for range 500 {
		e.Nudge()
	}
	deadline := time.Now().Add(10 * time.Second)
	for w.waits.Load() < 2 && time.Now().Before(deadline) {
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if got := w.waits.Load(); got != 2 {
		t.Fatalf("waiting wakes read %d times, want 2 (the first tick and one settled burst)", got)
	}
	if s := w.schedules.Load(); s != 1 {
		t.Fatalf("schedules read %d; want only the first tick's", s)
	}
}
