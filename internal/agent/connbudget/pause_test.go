package connbudget

import (
	"context"
	"errors"
	"sync"
	"testing"
	"time"
)

// waits records what a session was asked to wait for and moves the fake clock
// by it, so a pause costs no real time.
type waits struct {
	mu    sync.Mutex
	clock *fakeClock
	got   []time.Duration
	err   error
}

func (w *waits) Wait(_ context.Context, d time.Duration) error {
	w.mu.Lock()
	defer w.mu.Unlock()
	w.got = append(w.got, d)
	if w.err != nil {
		return w.err
	}
	w.clock.Advance(d)
	return nil
}

func (w *waits) recorded() []time.Duration {
	w.mu.Lock()
	defer w.mu.Unlock()
	return append([]time.Duration(nil), w.got...)
}

func openWaiting(t *testing.T, pool *Pool, clock *fakeClock, w *waits, cfg SessionConfig) *Session {
	t.Helper()
	cfg.Wait = w.Wait
	return newTestSession(t, pool, clock, cfg)
}

func TestRetryAfterIsWaitedOutBeforeTheNextRequest(t *testing.T) {
	clock := newClock()
	w := &waits{clock: clock}
	s := openWaiting(t, NewPool(8), clock, w, SessionConfig{Cap: 8, Fixed: 4})
	s.Throttled(7 * time.Second)
	lease, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	if got := w.recorded(); len(got) != 1 || got[0] != 7*time.Second {
		t.Fatalf("waited %v, want exactly the server's 7s", got)
	}
	// Once the time has passed nothing is waited for again.
	lease, err = s.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	if got := w.recorded(); len(got) != 1 {
		t.Fatalf("waited again after the pause had passed: %v", got)
	}
}

func TestAnAnswerThatNamesNoTimeStillPausesAndTheDelayGrows(t *testing.T) {
	clock := newClock()
	w := &waits{clock: clock}
	s := openWaiting(t, NewPool(8), clock, w, SessionConfig{Cap: 8, Fixed: 8})
	var want []time.Duration
	for _, d := range []time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 16 * time.Second, 30 * time.Second, 30 * time.Second} {
		clock.Advance(3 * time.Second) // beyond the backoff guard: each is its own event
		s.Throttled(0)
		lease, err := s.Acquire(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		lease.Release()
		want = append(want, d)
	}
	got := w.recorded()
	if len(got) != len(want) {
		t.Fatalf("waits = %v, want %v", got, want)
	}
	for i := range want {
		// The 3s the test moved the clock between answers counts toward the
		// pause already; what remains is the pause minus that.
		if got[i] > want[i] || got[i] < want[i]-3*time.Second {
			t.Fatalf("wait %d = %v, want about %v (%v)", i, got[i], want[i], got)
		}
	}
}

func TestAServersRetryAfterIsCappedAtTwoMinutes(t *testing.T) {
	clock := newClock()
	w := &waits{clock: clock}
	s := openWaiting(t, NewPool(8), clock, w, SessionConfig{Cap: 8, Fixed: 4})
	s.Throttled(24 * time.Hour)
	lease, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	if got := w.recorded(); len(got) != 1 || got[0] != MaxPause {
		t.Fatalf("waited %v, want the %v cap", got, MaxPause)
	}
}

func TestAPauseCoversEverySessionOfThePool(t *testing.T) {
	// Two pulls from the same registry: one told to wait, the other must not
	// ask the same address in the meantime either.
	clock := newClock()
	w := &waits{clock: clock}
	pool := NewPool(8)
	a := openWaiting(t, pool, clock, w, SessionConfig{Name: "a", Cap: 8, Fixed: 2})
	b := openWaiting(t, pool, clock, w, SessionConfig{Name: "b", Cap: 8, Fixed: 2})
	a.Throttled(9 * time.Second)
	lease, err := b.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	if got := w.recorded(); len(got) != 1 || got[0] != 9*time.Second {
		t.Fatalf("the other session waited %v, want 9s", got)
	}
	if b.Target() != 2 {
		t.Fatalf("the other session's target moved to %d: only the one that was refused backs off", b.Target())
	}
}

func TestACanceledWaitReturnsTheTokenAndTheError(t *testing.T) {
	clock := newClock()
	w := &waits{clock: clock, err: context.Canceled}
	pool := NewPool(1)
	s := openWaiting(t, pool, clock, w, SessionConfig{Cap: 1, Fixed: 1})
	s.Throttled(time.Second)
	if _, err := s.Acquire(context.Background()); !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire = %v, want the wait's error", err)
	}
	if pool.InUse() != 0 {
		t.Fatalf("%d tokens held by a request that never started", pool.InUse())
	}
}

func TestAPauseIsSpentHoldingNoToken(t *testing.T) {
	// The pause comes before queueing: a download that is waiting must not
	// sit on a token another download of the same address could use.
	clock := newClock()
	pool := NewPool(1)
	entered := make(chan struct{})
	release := make(chan struct{})
	a := newTestSession(t, pool, clock, SessionConfig{Name: "a", Cap: 1, Fixed: 1, Wait: func(ctx context.Context, d time.Duration) error {
		close(entered)
		select {
		case <-release:
			clock.Advance(d)
			return nil
		case <-ctx.Done():
			return ctx.Err()
		}
	}})
	a.Throttled(5 * time.Second)
	done := make(chan *Lease, 1)
	go func() {
		lease, _ := a.Acquire(context.Background())
		done <- lease
	}()
	<-entered
	if pool.InUse() != 0 {
		t.Fatalf("a request that is still waiting out a pause holds %d tokens", pool.InUse())
	}
	close(release)
	lease := <-done
	if pool.InUse() != 1 {
		t.Fatalf("pool in use = %d once the pause is over, want 1", pool.InUse())
	}
	lease.Release()
}
