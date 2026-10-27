package connbudget

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"strings"
	"sync"
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

// waitFor spins until cond holds. It is how a test waits for a goroutine to
// have reached a state it can observe; there is no sleep in it, and a state
// that never comes fails the test at the deadline.
func waitFor(t *testing.T, what string, cond func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !cond() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		runtime.Gosched()
	}
}

// queued is how many requests wait for a token.
func queued(s *Session) int {
	s.pool.mu.Lock()
	defer s.pool.mu.Unlock()
	return len(s.waiters)
}

// addBytes counts bytes toward the session's rate the way a connection's
// reads do.
func addBytes(s *Session, n int64) { s.bytes.Add(n) }

// demand keeps n requests asking for tokens: as many as the target allows are
// granted and held until stop, the rest wait. It is the state of a download
// with more chunks queued than connections, which is the only one in which a
// higher target can be measured. It returns once all n are either holding or
// queued.
func demand(t *testing.T, s *Session, n int) (stop func()) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	var wg sync.WaitGroup
	for i := 0; i < n; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease, err := s.Acquire(ctx)
			if err != nil {
				return
			}
			<-ctx.Done()
			lease.Release()
		}()
	}
	waitFor(t, "the requests to be holding or queued", func() bool { return s.InUse()+queued(s) == n })
	return func() {
		cancel()
		wg.Wait()
	}
}

// drive plays a source for steps windows: each window delivers what the source
// gives at the connections the session may hold, then the session decides.
func drive(s *Session, clock *fakeClock, window time.Duration, steps int, source func(conns int) float64) {
	for i := 0; i < steps; i++ {
		addBytes(s, int64(source(s.Target())*window.Seconds()))
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

// logRecorder collects a session's decisions as text.
type logRecorder struct {
	mu    sync.Mutex
	lines []string
}

func (r *logRecorder) log(msg string, kv ...any) {
	r.mu.Lock()
	defer r.mu.Unlock()
	line := msg
	for i := 0; i+1 < len(kv); i += 2 {
		line += fmt.Sprintf(" %v=%v", kv[i], kv[i+1])
	}
	r.lines = append(r.lines, line)
}

func (r *logRecorder) matching(substr string) []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	var out []string
	for _, line := range r.lines {
		if strings.Contains(line, substr) {
			out = append(out, line)
		}
	}
	return out
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
	drive(s, clock, 5*time.Second, 16, func(c int) float64 { return min(float64(c)*10*mb, 25*mb) })
	if got := s.Target(); got != 4 {
		t.Fatalf("target = %d, want 4: more connections brought no gain", got)
	}
}

func TestAPerAddressCapReachedAtTheStartNeverClimbs(t *testing.T) {
	clock := newClock()
	s := newTestSession(t, NewPool(32), clock, SessionConfig{Cap: 32})
	defer demand(t, s, 64)()
	drive(s, clock, 5*time.Second, 14, func(c int) float64 { return min(float64(c)*10*mb, 12*mb) })
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
		addBytes(s, 10*mb)
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
		t.Fatalf("target = %d, want the cap 8", got)
	}
}

func TestAFixedSessionNeverClimbs(t *testing.T) {
	clock := newClock()
	s := newTestSession(t, NewPool(32), clock, SessionConfig{Cap: 32, Fixed: 4})
	defer demand(t, s, 64)()
	drive(s, clock, 5*time.Second, 8, func(c int) float64 { return float64(c) * 10 * mb })
	if got := s.Target(); got != 4 {
		t.Fatalf("target = %d, want the fixed 4", got)
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
		rate := min(float64(s.Target())*10*mb, 25*mb)
		if step == 0 {
			rate = 2 * mb
		}
		addBytes(s, int64(rate*window.Seconds()))
		s.Tick(clock.Advance(window))
	}
	if got := s.Target(); got != 4 {
		t.Fatalf("target = %d, want 4: the plateau was already reached at the start", got)
	}
}

// rampSource is a link where each connection is capped (so more connections
// are more rate) but a newly opened one delivers nothing in the window it was
// opened in: TCP, TLS, the redirect and slow start take that long.
type rampSource struct {
	prev int // the target the last window ran at
}

func (r *rampSource) rate(target int) float64 {
	steady := float64(min(target, r.prev)) * 10 * mb // only connections that already existed
	r.prev = target
	return steady
}

func TestAClimbIsJudgedAfterTheNewConnectionsRampUp(t *testing.T) {
	// Without setting the ramp-up window aside a doubling is compared in the
	// window in which half the connections are still starting, shows no gain,
	// and the climb ends at the start on any link with latency.
	clock := newClock()
	s := newTestSession(t, NewPool(32), clock, SessionConfig{Cap: 32, Start: 4})
	defer demand(t, s, 64)()
	src := &rampSource{prev: 4}
	const maxWindows = 12 // warm-up, then two windows per doubling: 4 -> 8 -> 16 -> 32
	windows := 0
	for windows < maxWindows && s.Target() < 32 {
		addBytes(s, int64(src.rate(s.Target())*5))
		s.Tick(clock.Advance(5 * time.Second))
		windows++
	}
	if got := s.Target(); got != 32 {
		t.Fatalf("target = %d after %d windows, want the cap 32 within %d", got, windows, maxWindows)
	}
}

func TestANoisyWindowDoesNotEndTheClimbAtOnce(t *testing.T) {
	// One bad window in the middle of a climb (a stall on one connection)
	// must not settle the level for a minute: the doubling gets a second try.
	clock := newClock()
	s := newTestSession(t, NewPool(32), clock, SessionConfig{Cap: 32, Start: 4})
	defer demand(t, s, 64)()
	judged := 0 // windows run at a level above the start, after the ramp-up window
	for step := 0; step < 14; step++ {
		rate := float64(s.Target()) * 10 * mb
		if s.Target() == 8 {
			if judged++; judged == 2 { // the first judged window at 8 is the noisy one
				rate = 3 * mb
			}
		}
		addBytes(s, int64(rate*5))
		s.Tick(clock.Advance(5 * time.Second))
	}
	if got := s.Target(); got < 16 {
		t.Fatalf("target = %d: one noisy window ended the climb", got)
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

func TestABurstOfRefusalsPausesAsOneEventNotAsSixteen(t *testing.T) {
	// Sixteen simultaneous 429s that name no time: the pool waits the first
	// pause, not the thirty seconds sixteen escalations would add up to.
	clock := newClock()
	w := &waits{clock: clock}
	s := openWaiting(t, NewPool(32), clock, w, SessionConfig{Cap: 32, Start: 16})
	for i := 0; i < 16; i++ {
		s.Throttled(0)
	}
	lease, err := s.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	if got := w.recorded(); len(got) != 1 || got[0] != throttleBase {
		t.Fatalf("waited %v after a burst, want one wait of %v", got, throttleBase)
	}
	if got := s.Target(); got != 8 {
		t.Fatalf("target = %d, want 8", got)
	}
	// The next event, after the guard, is the second step of the doubling.
	clock.Advance(3 * time.Second)
	s.Throttled(0)
	lease, err = s.Acquire(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	lease.Release()
	if got := w.recorded(); len(got) != 2 || got[1] < 2*throttleBase-3*time.Second || got[1] > 2*throttleBase {
		t.Fatalf("waits %v, want the second to be about %v", got, 2*throttleBase)
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

func TestResetsAreCountedAgainstTheOpenConnections(t *testing.T) {
	// With sixteen connections open, a few resets in a window are routine;
	// half of them going down together is a limit.
	clock := newClock()
	s := newTestSession(t, NewPool(32), clock, SessionConfig{Cap: 32, Start: 16})
	defer demand(t, s, 16)()
	for i := 0; i < 7; i++ {
		s.Reset()
	}
	if got := s.Target(); got != 16 {
		t.Fatalf("7 resets among 16 connections moved the target to %d", got)
	}
	s.Reset()
	if got := s.Target(); got != 8 {
		t.Fatalf("8 resets among 16 connections: target = %d, want 8", got)
	}
}

func TestAHoldAfterResetsIsAtMostAMinute(t *testing.T) {
	// Repeated backoffs lengthen the hold up to eight minutes for throttling;
	// resets say less, and must not leave a long download crawling that long.
	clock := newClock()
	var rec logRecorder
	s := newTestSession(t, NewPool(32), clock, SessionConfig{Cap: 32, Start: 32, Log: rec.log})
	for i := 0; i < 6; i++ {
		clock.Advance(3 * time.Second)
		s.Reset()
		s.Reset()
		s.Reset()
	}
	lines := rec.matching("reason=connection resets")
	if len(lines) < 5 {
		t.Fatalf("only %d backoffs for resets: %v", len(lines), lines)
	}
	for _, line := range lines {
		if !strings.Contains(line, "hold=30s") && !strings.Contains(line, "hold=1m0s") {
			t.Fatalf("a reset backoff held for longer than a minute: %s", line)
		}
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

func TestThePoolCeilingIsSharedFairlyBetweenSessions(t *testing.T) {
	pool := NewPool(4)
	clock := newClock()
	a := newTestSession(t, pool, clock, SessionConfig{Name: "a", Cap: 32, Fixed: 8})
	b := newTestSession(t, pool, clock, SessionConfig{Name: "b", Cap: 32, Fixed: 8})
	stopA := demand(t, a, 20)
	defer stopA()
	if pool.InUse() != 4 || a.InUse() != 4 {
		t.Fatalf("pool in use %d (a: %d), want the whole ceiling of 4 taken by a", pool.InUse(), a.InUse())
	}
	// b starts asking while a holds every token and has a queue behind it.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	got := make(chan *Lease, 1)
	go func() {
		lease, err := b.Acquire(ctx)
		if err == nil {
			got <- lease
		}
	}()
	waitFor(t, "b to queue", func() bool { return queued(b) == 1 })
	// One of a's connections ends: the token goes to b, which has waited,
	// not to a's next queued request.
	a.pool.mu.Lock()
	var one *Lease
	for l := range a.leases {
		one = l
		break
	}
	a.pool.mu.Unlock()
	one.Release()
	lease := <-got
	if b.InUse() != 1 || pool.InUse() != 4 {
		t.Fatalf("b holds %d, pool %d: the freed token did not go to the second download", b.InUse(), pool.InUse())
	}
	lease.Release()
	if pool.InUse() > 4 {
		t.Fatalf("%d requests in flight, ceiling 4", pool.InUse())
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
	ctx, cancel := context.WithCancel(context.Background())
	errs := make(chan error, 1)
	go func() {
		_, err := s.Acquire(ctx)
		errs <- err
	}()
	waitFor(t, "the second request to queue", func() bool { return queued(s) == 1 })
	cancel()
	if err := <-errs; !errors.Is(err, context.Canceled) {
		t.Fatalf("Acquire = %v, want the context's error", err)
	}
	if queued(s) != 0 {
		t.Fatal("the canceled request is still queued")
	}
	lease.Release()
	lease.Release() // double release must not hand out a second token
	r1, err := s.Acquire(context.Background())
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
	waitFor(t, "the waiter to queue", func() bool { return queued(s) == 1 })
	s.Close()
	if err := <-errs; !errors.Is(err, ErrClosed) {
		t.Fatalf("waiter got %v, want ErrClosed", err)
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
	s.Throttled(time.Second)
	s.Reset()
	s.Tick(time.Now())
	s.Close()
	if s.Target() != 0 || s.InUse() != 0 || s.Bytes() != 0 || s.Ceiling() != 1 {
		t.Fatal("a nil session reports connections")
	}
}

func TestTheTickerGoroutineEvaluatesWithoutHelp(t *testing.T) {
	// The production path: the session's own goroutine ticks. The clock is
	// still the test's, so what it measures is exact; only the goroutine's
	// own wakeups are real.
	clock := newClock()
	pool := NewPool(32)
	var rec logRecorder
	s := pool.Open(SessionConfig{Cap: 8, Window: 5 * time.Second, Now: clock.Now, Log: rec.log})
	defer s.Close()
	defer demand(t, s, 32)()
	waitFor(t, "the ticker goroutine to raise the target to the cap", func() bool {
		addBytes(s, int64(float64(s.Target())*10*mb*5))
		clock.Advance(5 * time.Second)
		return s.Target() == 8
	})
	if len(rec.matching("connections changed")) == 0 {
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
	waitFor(t, "the second request to queue", func() bool { return queued(s) == 1 })
	if pool.InUse() != 1 {
		t.Fatalf("a second request ran with ceiling 1: in use %d", pool.InUse())
	}
	pool.SetCeiling(2)
	<-got
	if pool.Ceiling() != 2 {
		t.Fatalf("ceiling = %d", pool.Ceiling())
	}
	if s.Ceiling() != 2 {
		t.Fatalf("the session reports ceiling %d", s.Ceiling())
	}
}
