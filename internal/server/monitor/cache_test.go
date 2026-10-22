package monitor

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// manualClock is an injectable clock tests can advance deterministically,
// so TTL expiry doesn't depend on real wall-clock sleeps.
type manualClock struct {
	mu  sync.Mutex
	now time.Time
}

func newManualClock(start time.Time) *manualClock { return &manualClock{now: start} }

func (c *manualClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *manualClock) Advance(d time.Duration) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.now = c.now.Add(d)
}

func TestCachedReturnsFreshValueWithoutRecomputing(t *testing.T) {
	clock := newManualClock(baseTime)
	cache := NewCached[int](clock.Now, 10*time.Second)
	var calls int32

	compute := func(context.Context) (int, error) {
		atomic.AddInt32(&calls, 1)
		return 42, nil
	}

	for i := 0; i < 3; i++ {
		v, err := cache.Get(context.Background(), "cluster", "", "1h", compute)
		if err != nil || v != 42 {
			t.Fatalf("Get = %v, %v", v, err)
		}
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("compute calls = %d, want 1 (cached)", got)
	}
}

func TestCachedRecomputesAfterTTLExpires(t *testing.T) {
	clock := newManualClock(baseTime)
	cache := NewCached[int](clock.Now, 10*time.Second)
	var calls int32
	compute := func(context.Context) (int, error) {
		return int(atomic.AddInt32(&calls, 1)), nil
	}

	v1, _ := cache.Get(context.Background(), "cluster", "", "1h", compute)
	clock.Advance(11 * time.Second)
	v2, _ := cache.Get(context.Background(), "cluster", "", "1h", compute)

	if v1 != 1 || v2 != 2 {
		t.Fatalf("v1=%d v2=%d, want 1 then 2 (recomputed after TTL)", v1, v2)
	}
}

func TestCachedDistinguishesKeys(t *testing.T) {
	clock := newManualClock(baseTime)
	cache := NewCached[int](clock.Now, 10*time.Second)
	var calls int32
	compute := func(context.Context) (int, error) {
		return int(atomic.AddInt32(&calls, 1)), nil
	}

	a, _ := cache.Get(context.Background(), "service", "1", "1h", compute)
	b, _ := cache.Get(context.Background(), "service", "2", "1h", compute)
	if a == b {
		t.Fatalf("different ids should not share a cache entry: a=%d b=%d", a, b)
	}
}

func TestCachedSingleFlightsConcurrentCallers(t *testing.T) {
	clock := newManualClock(baseTime)
	cache := NewCached[int](clock.Now, 10*time.Second)
	var calls int32
	release := make(chan struct{})
	compute := func(context.Context) (int, error) {
		atomic.AddInt32(&calls, 1)
		<-release // block until the test lets every waiter pile up
		return 7, nil
	}

	const n = 5
	results := make([]int, n)
	var wg sync.WaitGroup
	wg.Add(n)
	for i := 0; i < n; i++ {
		go func(i int) {
			defer wg.Done()
			v, err := cache.Get(context.Background(), "cluster", "", "1h", compute)
			if err != nil {
				t.Errorf("Get: %v", err)
			}
			results[i] = v
		}(i)
	}

	// Give every goroutine a chance to reach cache.Get and block inside
	// compute before releasing them.
	time.Sleep(50 * time.Millisecond)
	close(release)
	wg.Wait()

	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("compute calls = %d, want 1 (single-flighted)", got)
	}
	for i, v := range results {
		if v != 7 {
			t.Errorf("results[%d] = %d, want 7", i, v)
		}
	}
}

func TestCachedPropagatesComputeError(t *testing.T) {
	clock := newManualClock(baseTime)
	cache := NewCached[int](clock.Now, 10*time.Second)
	wantErr := context.DeadlineExceeded
	compute := func(context.Context) (int, error) { return 0, wantErr }

	_, err := cache.Get(context.Background(), "cluster", "", "1h", compute)
	if err != wantErr {
		t.Fatalf("err = %v, want %v", err, wantErr)
	}
}

// TestCachedSweepsExpiredEntries is F11's regression case: a key that stops
// being requested (a deleted deployment, say) must not sit in the cache
// forever. Every Get call sweeps other completed, expired entries out of the
// map.
func TestCachedSweepsExpiredEntries(t *testing.T) {
	clock := newManualClock(baseTime)
	cache := NewCached[int](clock.Now, 10*time.Second)
	compute := func(context.Context) (int, error) { return 1, nil }

	for _, id := range []string{"1", "2", "3"} {
		if _, err := cache.Get(context.Background(), "service", id, "1h", compute); err != nil {
			t.Fatalf("Get(%s): %v", id, err)
		}
	}
	if got := len(cache.entries); got != 3 {
		t.Fatalf("entries = %d, want 3 before expiry", got)
	}

	clock.Advance(11 * time.Second) // past the 10s TTL
	if _, err := cache.Get(context.Background(), "service", "4", "1h", compute); err != nil {
		t.Fatalf("Get(4): %v", err)
	}

	if got := len(cache.entries); got != 1 {
		t.Fatalf("entries = %d, want 1 (only the fresh key 4; 1/2/3 swept)", got)
	}
	if _, ok := cache.entries[cacheKey{kind: "service", id: "4", rng: "1h"}]; !ok {
		t.Fatal("expected key 4's entry to remain")
	}
}

// TestCachedSweepNeverRemovesInFlightEntry guards the sweep against removing
// an entry whose computation hasn't finished yet - deleting it out from
// under its waiters would leave them blocked on a channel nothing ever
// closes.
func TestCachedSweepNeverRemovesInFlightEntry(t *testing.T) {
	clock := newManualClock(baseTime)
	cache := NewCached[int](clock.Now, 10*time.Second)
	release := make(chan struct{})
	inFlight := make(chan struct{})
	compute := func(context.Context) (int, error) {
		close(inFlight)
		<-release
		return 1, nil
	}

	go func() { _, _ = cache.Get(context.Background(), "cluster", "", "1h", compute) }()
	<-inFlight
	clock.Advance(11 * time.Second) // past the TTL, but the entry has no "at" yet

	done := make(chan struct{})
	var got int
	var getErr error
	go func() {
		got, getErr = cache.Get(context.Background(), "cluster", "", "1h", func(context.Context) (int, error) {
			t.Error("a second compute must not run while the first is in flight")
			return -1, nil
		})
		close(done)
	}()

	close(release)
	<-done
	if getErr != nil || got != 1 {
		t.Fatalf("Get = %v, %v, want the in-flight computation's result (1, nil)", got, getErr)
	}
}

// TestCachedNeverStoresACancelledLeaderResult is the cache-poisoning
// blocker's monitor-side half: a leader whose own compute call returns
// context.Canceled (its request was aborted mid-computation) must not leave
// that error sitting in the cache for the next caller. The next Get for the
// same key must trigger a brand new compute call rather than replaying the
// cancellation.
func TestCachedNeverStoresACancelledLeaderResult(t *testing.T) {
	clock := newManualClock(baseTime)
	cache := NewCached[int](clock.Now, 10*time.Second)
	var calls int32
	compute := func(context.Context) (int, error) {
		n := atomic.AddInt32(&calls, 1)
		if n == 1 {
			return 0, context.Canceled
		}
		return 99, nil
	}

	_, err := cache.Get(context.Background(), "cluster", "", "1h", compute)
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("first Get err = %v, want context.Canceled", err)
	}
	if got := len(cache.entries); got != 0 {
		t.Fatalf("entries = %d, want 0: a cancelled leader result must not be stored", got)
	}

	// No clock advance at all: if the cancellation had been cached (even
	// under the short negative TTL), this call would still see it.
	v, err := cache.Get(context.Background(), "cluster", "", "1h", compute)
	if err != nil || v != 99 {
		t.Fatalf("second Get = %v, %v, want a fresh (uncached) computation to run", v, err)
	}
	if got := atomic.LoadInt32(&calls); got != 2 {
		t.Fatalf("compute calls = %d, want 2 (cancellation must not be reused)", got)
	}
}

// TestCachedDeadlineExceededIsNeverStoredEither is the same guarantee for
// the other stdlib context error Get treats as "the caller's problem, not
// the computation's".
func TestCachedDeadlineExceededIsNeverStoredEither(t *testing.T) {
	clock := newManualClock(baseTime)
	cache := NewCached[int](clock.Now, 10*time.Second)
	compute := func(context.Context) (int, error) { return 0, context.DeadlineExceeded }

	if _, err := cache.Get(context.Background(), "cluster", "", "1h", compute); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err = %v, want context.DeadlineExceeded", err)
	}
	if got := len(cache.entries); got != 0 {
		t.Fatalf("entries = %d, want 0", got)
	}
}

// TestCachedOrdinaryErrorExpiresQuickly proves the negative-TTL half of the
// fix independently of context cancellation: a plain backend error (a tsdb
// query failure, say) is still cached briefly - so a genuinely broken
// backend does not get hammered by every poller - but for negativeTTL, not
// the much longer success ttl.
func TestCachedOrdinaryErrorExpiresQuickly(t *testing.T) {
	clock := newManualClock(baseTime)
	cache := NewCached[int](clock.Now, 10*time.Second)
	wantErr := errors.New("tsdb unavailable")
	var calls int32
	compute := func(context.Context) (int, error) {
		if atomic.AddInt32(&calls, 1) == 1 {
			return 0, wantErr
		}
		return 5, nil
	}

	if _, err := cache.Get(context.Background(), "cluster", "", "1h", compute); !errors.Is(err, wantErr) {
		t.Fatalf("first err = %v, want %v", err, wantErr)
	}
	// Still within the 1s negative TTL: the error must still be cached
	// (no second compute call yet).
	clock.Advance(500 * time.Millisecond)
	if _, err := cache.Get(context.Background(), "cluster", "", "1h", compute); !errors.Is(err, wantErr) {
		t.Fatalf("err within negative TTL = %v, want the cached %v", err, wantErr)
	}
	if got := atomic.LoadInt32(&calls); got != 1 {
		t.Fatalf("compute calls = %d within negative TTL, want 1", got)
	}
	// Past the 1s negative TTL (but nowhere near the 10s success TTL): a
	// fresh attempt must run.
	clock.Advance(600 * time.Millisecond)
	v, err := cache.Get(context.Background(), "cluster", "", "1h", compute)
	if err != nil || v != 5 {
		t.Fatalf("Get after negative TTL = %v, %v, want a fresh successful attempt", v, err)
	}
}

func TestCachedContextCancelledWhileWaiting(t *testing.T) {
	clock := newManualClock(baseTime)
	cache := NewCached[int](clock.Now, 10*time.Second)
	release := make(chan struct{})
	compute := func(context.Context) (int, error) {
		<-release
		return 1, nil
	}

	go func() { _, _ = cache.Get(context.Background(), "cluster", "", "1h", compute) }()
	time.Sleep(20 * time.Millisecond)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := cache.Get(ctx, "cluster", "", "1h", compute)
	if err == nil {
		t.Fatal("expected an error for an already-cancelled context while waiting")
	}
	close(release)
}
