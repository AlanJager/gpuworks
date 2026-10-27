package connbudget

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

const mb = 1 << 20

// fakeClock is the session's clock; the tests advance it by hand.
type fakeClock struct {
	mu sync.Mutex
	t  time.Time
}

func newClock() *fakeClock { return &fakeClock{t: time.Unix(1_800_000_000, 0)} }

func (c *fakeClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.t
}

func (c *fakeClock) Advance(d time.Duration) time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.t = c.t.Add(d)
	return c.t
}

// demand keeps n requests queued on the session, each held for a moment, the
// way a repository's chunks keep asking for connections.
func demand(t *testing.T, s *Session, n int) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for {
				lease, err := s.Acquire(ctx)
				if err != nil {
					return
				}
				time.Sleep(time.Millisecond)
				lease.Release()
			}
		}()
	}
	return func() {
		cancel()
		wg.Wait()
	}
}

// drive plays a source for steps windows: each window delivers what the source
// gives at the connections the session may hold, then the session decides.
func drive(s *Session, clock *fakeClock, window time.Duration, steps int, source func(conns int) float64) {
	for i := 0; i < steps; i++ {
		time.Sleep(15 * time.Millisecond) // let queued requests take freed tokens
		conns := s.Target()
		s.AddBytes(int(source(conns) * window.Seconds()))
		s.Tick(clock.Advance(window))
	}
}

func newTestSession(t *testing.T, pool *Pool, clock *fakeClock, cfg SessionConfig) *Session {
	t.Helper()
	cfg.Now, cfg.NoTicker = clock.Now, true
	if cfg.Wait == nil {
		// Pauses cost no real time; the fake clock is advanced by the test.
		cfg.Wait = func(ctx context.Context, _ time.Duration) error { return ctx.Err() }
	}
	if cfg.Window == 0 {
		cfg.Window = 5 * time.Second
	}
	s := pool.Open(cfg)
	t.Cleanup(s.Close)
	return s
}

func TestClimbsWhileEveryConnectionAddsRate(t *testing.T) {
	// A per-connection cap, as one throttled TCP flow gives: rate grows with
	// connections until the session's own cap.
	clock := newClock()
	s := newTestSession(t, NewPool(32), clock, SessionConfig{Cap: 32})
	defer demand(t, s, 64)()
	drive(s, clock, 5*time.Second, 14, func(c int) float64 { return float64(c) * 10 * mb })
	if got := s.Target(); got != 32 {
		t.Fatalf("target = %d, want the cap 32: every doubling still paid", got)
	}
}

func TestStopsAtThePlateauOfAnAggregateCap(t *testing.T) {
	// A per-address bandwidth cap of 25 MB/s: four connections already deliver
	// it, eight add nothing, so the climb must settle at four.
	clock := newClock()
	s := newTestSession(t, NewPool(32), clock, SessionConfig{Cap: 32})
	defer demand(t, s, 64)()
	drive(s, clock, 5*time.Second, 14, func(c int) float64 { return min(float64(c)*10*mb, 25*mb) })
	if got := s.Target(); got != 4 {
		t.Fatalf("target = %d, want 4: more connections brought no gain", got)
	}
}

func TestAPerAddressCapReachedAtTheStartNeverClimbs(t *testing.T) {
	clock := newClock()
	s := newTestSession(t, NewPool(32), clock, SessionConfig{Cap: 32})
	defer demand(t, s, 64)()
	drive(s, clock, 5*time.Second, 12, func(c int) float64 { return min(float64(c)*10*mb, 12*mb) })
	if got := s.Target(); got != DefaultStart {
		t.Fatalf("target = %d, want the start %d", got, DefaultStart)
	}
}

func TestAnIdleSessionLearnsNothing(t *testing.T) {
	// Fewer requests wanted than the target (the tail of a repository): the
	// rate says nothing about connections, so the target must not move.
	clock := newClock()
	s := newTestSession(t, NewPool(32), clock, SessionConfig{Cap: 32})
	for i := 0; i < 6; i++ {
		s.AddBytes(10 * mb)
		s.Tick(clock.Advance(5 * time.Second))
	}
	if got := s.Target(); got != DefaultStart {
		t.Fatalf("target = %d, want %d", got, DefaultStart)
	}
}

func TestACapBoundsTheClimb(t *testing.T) {
	clock := newClock()
	s := newTestSession(t, NewPool(32), clock, SessionConfig{Cap: 8})
	defer demand(t, s, 64)()
	drive(s, clock, 5*time.Second, 12, func(c int) float64 { return float64(c) * 10 * mb })
	if got := s.Target(); got != 8 {
		t.Fatalf("target = %d, want the mirror cap 8", got)
	}
}

func TestAFixedSessionNeverClimbs(t *testing.T) {
	clock := newClock()
	s := newTestSession(t, NewPool(32), clock, SessionConfig{Cap: 32, Fixed: TodayConnections})
	defer demand(t, s, 64)()
	drive(s, clock, 5*time.Second, 8, func(c int) float64 { return float64(c) * 10 * mb })
	if got := s.Target(); got != TodayConnections {
		t.Fatalf("target = %d, want the fixed %d", got, TodayConnections)
	}
}

func TestThrottlingHalvesDownToOneAndOneBurstIsOneEvent(t *testing.T) {
	clock := newClock()
	s := newTestSession(t, NewPool(32), clock, SessionConfig{Cap: 32, Start: 16})
	if got := s.Target(); got != 16 {
		t.Fatalf("start = %d", got)
	}
	// Sixteen connections all answered 429 at once: one halving, not sixteen.
	for i := 0; i < 16; i++ {
		s.Throttled(0)
	}
	if got := s.Target(); got != 8 {
		t.Fatalf("after one burst target = %d, want 8", got)
	}
	for _, want := range []int{4, 2, 1, 1} {
		clock.Advance(3 * time.Second)
		s.Throttled(0)
		if got := s.Target(); got != want {
			t.Fatalf("target = %d, want %d", got, want)
		}
	}
}

func TestAThrottledSessionStaysBelowTheLevelThatTriggeredIt(t *testing.T) {
	clock := newClock()
	s := newTestSession(t, NewPool(32), clock, SessionConfig{Cap: 32, Start: 16})
	defer demand(t, s, 64)()
	s.Throttled(0)
	// Plenty of room in the source, but the session was told to slow down.
	drive(s, clock, 5*time.Second, 20, func(c int) float64 { return float64(c) * 10 * mb })
	if got := s.Target(); got > 8 {
		t.Fatalf("target = %d: climbed back above the level that was throttled (8) within %v", got, 100*time.Second)
	}
}

func TestAThrottledSessionEarnsItsCeilingBackAfterACalmSpell(t *testing.T) {
	clock := newClock()
	s := newTestSession(t, NewPool(32), clock, SessionConfig{Cap: 32, Start: 16})
	defer demand(t, s, 64)()
	s.Throttled(0)
	// Five virtual minutes of a healthy source.
	drive(s, clock, 5*time.Second, 60, func(c int) float64 { return float64(c) * 10 * mb })
	if got := s.Target(); got <= 8 {
		t.Fatalf("target = %d: a one-off 429 must not cap a long download for good", got)
	}
}

func TestResetsBackOffOnlyWhenRepeated(t *testing.T) {
	clock := newClock()
	s := newTestSession(t, NewPool(32), clock, SessionConfig{Cap: 32, Start: 8})
	s.Reset()
	s.Reset()
	if got := s.Target(); got != 8 {
		t.Fatalf("two resets moved the target to %d: a long download sees an occasional one", got)
	}
	s.Reset()
	if got := s.Target(); got != 4 {
		t.Fatalf("three resets: target = %d, want 4", got)
	}
	// Resets are counted per window.
	clock.Advance(10 * time.Second)
	s.Tick(clock.Now())
	s.Reset()
	s.Reset()
	if got := s.Target(); got != 4 {
		t.Fatalf("resets in different windows were added up: target = %d", got)
	}
}

func TestRepeatedResetsReachOneConnection(t *testing.T) {
	clock := newClock()
	s := newTestSession(t, NewPool(32), clock, SessionConfig{Cap: 32, Start: 8})
	for i := 0; i < 12; i++ {
		clock.Advance(3 * time.Second)
		s.Reset()
		s.Reset()
		s.Reset()
	}
	if got := s.Target(); got != 1 {
		t.Fatalf("target = %d, want 1", got)
	}
}

func TestNoRangeIsOneConnectionForGood(t *testing.T) {
	clock := newClock()
	s := newTestSession(t, NewPool(32), clock, SessionConfig{Cap: 32, Start: 8})
	defer demand(t, s, 16)()
	s.NoRange()
	s.SetCap(32)
	drive(s, clock, 5*time.Second, 10, func(c int) float64 { return float64(c) * 10 * mb })
	if got := s.Target(); got != 1 {
		t.Fatalf("target = %d, want 1", got)
	}
	time.Sleep(20 * time.Millisecond)
	if got := s.InUse(); got > 1 {
		t.Fatalf("%d connections in use, want at most 1", got)
	}
}

func TestThePoolCeilingIsSharedFairlyBetweenSessions(t *testing.T) {
	pool := NewPool(4)
	clock := newClock()
	a := newTestSession(t, pool, clock, SessionConfig{Name: "a", Cap: 32, Fixed: 8})
	b := newTestSession(t, pool, clock, SessionConfig{Name: "b", Cap: 32, Fixed: 8})
	stopA := demand(t, a, 20)
	defer stopA()
	time.Sleep(30 * time.Millisecond)
	stopB := demand(t, b, 20)
	defer stopB()
	var maxInUse, bGot atomic.Int64
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if in := int64(pool.InUse()); in > maxInUse.Load() {
			maxInUse.Store(in)
		}
		if got := int64(b.InUse()); got > bGot.Load() {
			bGot.Store(got)
		}
		time.Sleep(time.Millisecond)
	}
	if maxInUse.Load() > 4 {
		t.Fatalf("%d requests in flight, ceiling 4", maxInUse.Load())
	}
	if bGot.Load() < 1 {
		t.Fatal("the second download never got a connection while the first held a queue")
	}
}

func TestACanceledAcquireLeaksNoToken(t *testing.T) {
	pool := NewPool(1)
	clock := newClock()
	s := newTestSession(t, pool, clock, SessionConfig{Cap: 1, Fixed: 1})
	lease, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 30*time.Millisecond)
	defer cancel()
	if _, err := s.Acquire(ctx); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Acquire = %v, want the context's error", err)
	}
	lease.Release()
	lease.Release() // double release must not hand out a second token
	ctx2, cancel2 := context.WithTimeout(context.Background(), time.Second)
	defer cancel2()
	r1, err := s.Acquire(ctx2)
	if err != nil {
		t.Fatalf("token lost: %v", err)
	}
	defer r1.Release()
	if got := pool.InUse(); got != 1 {
		t.Fatalf("pool in use = %d, want 1", got)
	}
}

func TestClosingASessionFailsItsWaiters(t *testing.T) {
	pool := NewPool(1)
	clock := newClock()
	s := newTestSession(t, pool, clock, SessionConfig{Cap: 1, Fixed: 1})
	hold, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Release()
	errs := make(chan error, 1)
	go func() {
		_, err := s.Acquire(context.Background())
		errs <- err
	}()
	time.Sleep(20 * time.Millisecond)
	s.Close()
	select {
	case err := <-errs:
		if !errors.Is(err, ErrClosed) {
			t.Fatalf("waiter got %v, want ErrClosed", err)
		}
	case <-time.After(time.Second):
		t.Fatal("a waiter outlived its session")
	}
	if _, err := s.Acquire(context.Background()); !errors.Is(err, ErrClosed) {
		t.Fatalf("Acquire after Close = %v", err)
	}
}

func TestANilSessionAdmitsEverything(t *testing.T) {
	var s *Session
	lease, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if lease.Observe(100) {
		t.Fatal("a lease of a nil session judged a connection")
	}
	lease.Release()
	s.AddBytes(10)
	s.Throttled(time.Second)
	s.Reset()
	s.NoRange()
	s.SetCap(3)
	s.Tick(time.Now())
	s.Close()
	if s.Target() != 0 || s.InUse() != 0 {
		t.Fatal("a nil session reports connections")
	}
}

func TestTheTickerGoroutineEvaluatesWithoutHelp(t *testing.T) {
	// The production path: a real clock and the session's own goroutine.
	pool := NewPool(32)
	var lines atomic.Int64
	s := pool.Open(SessionConfig{Cap: 8, Window: 60 * time.Millisecond, Log: func(string, ...any) { lines.Add(1) }})
	defer s.Close()
	stop := demand(t, s, 32)
	defer stop()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) && s.Target() < 8 {
		s.AddBytes(int(float64(s.Target()) * 10 * mb * 0.02))
		time.Sleep(20 * time.Millisecond)
	}
	if got := s.Target(); got != 8 {
		t.Fatalf("target = %d, want 8", got)
	}
	if lines.Load() == 0 {
		t.Fatal("no decision was logged")
	}
}

func TestSetCeilingAppliesToQueuedRequests(t *testing.T) {
	pool := NewPool(1)
	clock := newClock()
	s := newTestSession(t, pool, clock, SessionConfig{Cap: 32, Fixed: 4})
	hold, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	defer hold.Release()
	got := make(chan struct{})
	go func() {
		lease, err := s.Acquire(context.Background())
		if err == nil {
			lease.Release()
			close(got)
		}
	}()
	select {
	case <-got:
		t.Fatal("a second request ran with ceiling 1")
	case <-time.After(30 * time.Millisecond):
	}
	pool.SetCeiling(2)
	select {
	case <-got:
	case <-time.After(time.Second):
		t.Fatal("raising the ceiling did not admit the queued request")
	}
	if pool.Ceiling() != 2 {
		t.Fatalf("ceiling = %d", pool.Ceiling())
	}
}

func TestTheFirstWindowIsNoBaselineBecauseConnectionsAreStillStartingUp(t *testing.T) {
	// The first window delivers little (handshakes, redirects); from then on
	// four connections already fill the address. Measured against the slow
	// first window, doubling would look like a big gain and the climb would
	// run away to the cap.
	clock := newClock()
	s := newTestSession(t, NewPool(32), clock, SessionConfig{Cap: 32, Start: 4})
	defer demand(t, s, 64)()
	window := 5 * time.Second
	for step := 0; step < 14; step++ {
		time.Sleep(15 * time.Millisecond)
		rate := min(float64(s.Target())*10*mb, 25*mb)
		if step == 0 {
			rate = 2 * mb
		}
		s.AddBytes(int(rate * window.Seconds()))
		s.Tick(clock.Advance(window))
	}
	if got := s.Target(); got != 4 {
		t.Fatalf("target = %d, want 4: the plateau was already reached at the start", got)
	}
}
