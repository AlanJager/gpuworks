package monitor

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// The tests below cover Cached's stale-while-revalidate behaviour: a read of
// an expired value returns it at once and refreshes it in the background.

var swrKey = cacheKey{kind: "cluster", rng: "1h"}

// within fails the test if f does not return in time: it is how a test proves
// that a call did not wait for something it was told not to wait for.
func within(t *testing.T, what string, f func()) {
	t.Helper()
	done := make(chan struct{})
	go func() {
		defer close(done)
		f()
	}()
	select {
	case <-done:
	case <-time.After(5 * time.Second):
		t.Fatalf("%s did not return while the refresh was blocked", what)
	}
}

// awaitRefresh waits until the refresh running for key (if any) has finished,
// whether it replaced the value or failed.
func (c *Cached[T]) awaitRefresh(t *testing.T, key cacheKey) {
	t.Helper()
	c.mu.Lock()
	started := c.entries[key]
	c.mu.Unlock()
	deadline := time.Now().Add(5 * time.Second)
	for {
		c.mu.Lock()
		cur := c.entries[key]
		finished := cur != started || cur == nil || !cur.refreshing
		c.mu.Unlock()
		if finished {
			return
		}
		if time.Now().After(deadline) {
			t.Fatal("the refresh did not finish")
		}
		time.Sleep(time.Millisecond)
	}
}

func (c *Cached[T]) has(key cacheKey) bool {
	c.mu.Lock()
	defer c.mu.Unlock()
	_, ok := c.entries[key]
	return ok
}

// blockingBuilder numbers its builds 1, 2, ...; build n >= blockFrom waits
// for release before it returns, after announcing itself on started.
type blockingBuilder struct {
	calls     atomic.Int32
	blockFrom int32
	started   chan struct{}
	release   chan struct{}
}

func newBlockingBuilder(blockFrom int32) *blockingBuilder {
	return &blockingBuilder{blockFrom: blockFrom, started: make(chan struct{}, 16), release: make(chan struct{})}
}

func (b *blockingBuilder) build(context.Context) (int, error) {
	n := b.calls.Add(1)
	if n >= b.blockFrom {
		b.started <- struct{}{}
		<-b.release
	}
	return int(n), nil
}

// An expired read returns the previous value, with the time it was computed,
// without waiting for the rebuild; the rebuild replaces it once it is done.
func TestCachedStaleReadDoesNotWaitForTheRefresh(t *testing.T) {
	clock := newManualClock(baseTime)
	cache := NewCached[int](clock.Now, 10*time.Second)
	b := newBlockingBuilder(2)

	v, at1, err := cache.GetAt(context.Background(), "cluster", "", "1h", b.build)
	if err != nil || v != 1 {
		t.Fatalf("first GetAt = %v, %v, want a synchronous build (1, nil)", v, err)
	}
	clock.Advance(11 * time.Second)

	var got int
	var gotAt time.Time
	within(t, "the stale read", func() {
		got, gotAt, err = cache.GetAt(context.Background(), "cluster", "", "1h", b.build)
	})
	if err != nil || got != 1 || !gotAt.Equal(at1) {
		t.Fatalf("stale GetAt = %v at %v (%v), want the old value 1 at %v", got, gotAt, err, at1)
	}
	<-b.started // the refresh is running, and still blocked

	close(b.release)
	cache.awaitRefresh(t, swrKey)
	got, gotAt, err = cache.GetAt(context.Background(), "cluster", "", "1h", b.build)
	if err != nil || got != 2 || !gotAt.After(at1) {
		t.Fatalf("GetAt after the refresh = %v at %v (%v), want the new value 2 after %v", got, gotAt, err, at1)
	}
}

// Many readers of an expired key start exactly one refresh between them, and
// all get the old value.
func TestCachedConcurrentStaleReadsStartOneRefresh(t *testing.T) {
	clock := newManualClock(baseTime)
	cache := NewCached[int](clock.Now, 10*time.Second)
	b := newBlockingBuilder(2)
	if _, err := cache.Get(context.Background(), "cluster", "", "1h", b.build); err != nil {
		t.Fatal(err)
	}
	clock.Advance(11 * time.Second)

	const readers = 32
	results := make([]int, readers)
	var wg sync.WaitGroup
	wg.Add(readers)
	for i := 0; i < readers; i++ {
		go func(i int) {
			defer wg.Done()
			v, err := cache.Get(context.Background(), "cluster", "", "1h", b.build)
			if err != nil {
				t.Errorf("Get: %v", err)
			}
			results[i] = v
		}(i)
	}
	within(t, "the stale reads", wg.Wait)
	<-b.started
	for i, v := range results {
		if v != 1 {
			t.Errorf("results[%d] = %d, want the old value 1", i, v)
		}
	}
	close(b.release)
	cache.awaitRefresh(t, swrKey)
	if got := b.calls.Load(); got != 2 {
		t.Fatalf("builds = %d, want 2 (the first, and exactly one refresh)", got)
	}
}

// A failed refresh is not shown: the old value keeps being served, no new
// attempt is made until negativeTTL has passed, and a read after that tries
// again and replaces the value when it works.
func TestCachedFailedRefreshKeepsServingTheOldValueAndRetries(t *testing.T) {
	clock := newManualClock(baseTime)
	cache := NewCached[int](clock.Now, 10*time.Second)
	var calls atomic.Int32
	build := func(context.Context) (int, error) {
		switch n := calls.Add(1); n {
		case 2:
			return 0, errors.New("tsdb unavailable")
		default:
			return int(n), nil
		}
	}
	get := func() int {
		t.Helper()
		v, err := cache.Get(context.Background(), "cluster", "", "1h", build)
		if err != nil {
			t.Fatalf("Get returned the refresh's error: %v", err)
		}
		return v
	}

	if v := get(); v != 1 {
		t.Fatalf("first = %d, want 1", v)
	}
	clock.Advance(11 * time.Second)
	if v := get(); v != 1 { // starts the refresh that fails
		t.Fatalf("stale = %d, want 1", v)
	}
	cache.awaitRefresh(t, swrKey)
	if v := get(); v != 1 {
		t.Fatalf("after the failed refresh = %d, want the old value 1", v)
	}
	if got := calls.Load(); got != 2 {
		t.Fatalf("builds = %d right after the failure, want 2 (no retry within negativeTTL)", got)
	}

	clock.Advance(negativeTTL + time.Millisecond)
	if v := get(); v != 1 { // starts the retry
		t.Fatalf("stale again = %d, want 1", v)
	}
	cache.awaitRefresh(t, swrKey)
	if v := get(); v != 3 {
		t.Fatalf("after the retry = %d, want 3", v)
	}
}

// A panic in the background refresh does not crash the process: it counts as
// a failed refresh.
func TestCachedRefreshPanicIsRecovered(t *testing.T) {
	clock := newManualClock(baseTime)
	cache := NewCached[int](clock.Now, 10*time.Second)
	var calls atomic.Int32
	build := func(context.Context) (int, error) {
		n := calls.Add(1)
		if n == 2 {
			panic("boom")
		}
		return int(n), nil
	}
	if _, err := cache.Get(context.Background(), "cluster", "", "1h", build); err != nil {
		t.Fatal(err)
	}
	clock.Advance(11 * time.Second)
	if v, err := cache.Get(context.Background(), "cluster", "", "1h", build); err != nil || v != 1 {
		t.Fatalf("stale Get = %v, %v, want 1, nil", v, err)
	}
	cache.awaitRefresh(t, swrKey)
	clock.Advance(negativeTTL + time.Millisecond)
	if _, err := cache.Get(context.Background(), "cluster", "", "1h", build); err != nil {
		t.Fatal(err)
	}
	cache.awaitRefresh(t, swrKey)
	if v, _ := cache.Get(context.Background(), "cluster", "", "1h", build); v != 3 {
		t.Fatalf("after the panic and a retry = %d, want 3", v)
	}
}

// The refresh runs under a context of its own: not canceled when the request
// that started it is, and bounded by a deadline.
func TestCachedRefreshContextIsNotTheRequests(t *testing.T) {
	clock := newManualClock(baseTime)
	cache := NewCached[int](clock.Now, 10*time.Second)
	if _, err := cache.Get(context.Background(), "cluster", "", "1h", func(context.Context) (int, error) { return 1, nil }); err != nil {
		t.Fatal(err)
	}
	clock.Advance(11 * time.Second)

	type seen struct {
		err         error
		hasDeadline bool
	}
	ctxSeen := make(chan seen, 1)
	proceed := make(chan struct{})
	build := func(ctx context.Context) (int, error) {
		<-proceed
		_, ok := ctx.Deadline()
		ctxSeen <- seen{err: ctx.Err(), hasDeadline: ok}
		return 2, nil
	}
	reqCtx, cancel := context.WithCancel(context.Background())
	if _, err := cache.Get(reqCtx, "cluster", "", "1h", build); err != nil {
		t.Fatal(err)
	}
	cancel() // the request is over before the refresh gets going
	close(proceed)
	got := <-ctxSeen
	if got.err != nil || !got.hasDeadline {
		t.Fatalf("refresh context: err=%v deadline=%v, want alive and bounded", got.err, got.hasDeadline)
	}
	cache.awaitRefresh(t, swrKey)
}

// A value too old to show (retainTTLs TTLs) is not served stale: the reader
// waits for a new one, as for a key never computed.
func TestCachedValueTooOldToShowIsRebuiltSynchronously(t *testing.T) {
	clock := newManualClock(baseTime)
	cache := NewCached[int](clock.Now, 10*time.Second)
	var calls atomic.Int32
	build := func(context.Context) (int, error) { return int(calls.Add(1)), nil }
	if _, err := cache.Get(context.Background(), "cluster", "", "1h", build); err != nil {
		t.Fatal(err)
	}
	clock.Advance(retainTTLs*10*time.Second + time.Second)
	v, err := cache.Get(context.Background(), "cluster", "", "1h", build)
	if err != nil || v != 2 {
		t.Fatalf("Get = %v, %v, want the rebuilt 2", v, err)
	}
}

// Keys nobody reads go away; keys that are read stay.
func TestCachedEvictsIdleKeys(t *testing.T) {
	clock := newManualClock(baseTime)
	cache := NewCached[int](clock.Now, 10*time.Second)
	build := func(context.Context) (int, error) { return 1, nil }
	hot := cacheKey{kind: "service", id: "hot", rng: "1h"}
	idle := cacheKey{kind: "service", id: "idle", rng: "1h"}
	for _, k := range []cacheKey{hot, idle} {
		if _, err := cache.Get(context.Background(), k.kind, k.id, k.rng, build); err != nil {
			t.Fatal(err)
		}
	}

	for i := 0; i < retainTTLs+1; i++ {
		clock.Advance(10 * time.Second)
		if _, err := cache.Get(context.Background(), hot.kind, hot.id, hot.rng, build); err != nil {
			t.Fatal(err)
		}
		cache.awaitRefresh(t, hot)
	}
	if cache.has(idle) {
		t.Fatal("a key nobody read for more than retainTTLs TTLs is still cached")
	}
	if !cache.has(hot) {
		t.Fatal("a key that keeps being read was evicted")
	}
}

// With a TTL of 0 (the picked-range views) nothing is kept: every read
// computes, none is served from before, and no refresh runs behind it.
func TestCachedZeroTTLNeverCaches(t *testing.T) {
	clock := newManualClock(baseTime)
	cache := NewCached[int](clock.Now, 0)
	var calls atomic.Int32
	build := func(context.Context) (int, error) { return int(calls.Add(1)), nil }
	for want := 1; want <= 3; want++ {
		v, err := cache.Get(context.Background(), "cluster", "", "1h", build)
		if err != nil || v != want {
			t.Fatalf("Get #%d = %v, %v, want a fresh computation (%d)", want, v, err, want)
		}
	}
	if got := calls.Load(); got != 3 {
		t.Fatalf("builds = %d, want 3", got)
	}
	// What was computed is not kept: the next read of any key sweeps it.
	if _, err := cache.Get(context.Background(), "cluster", "", "6h", build); err != nil {
		t.Fatal(err)
	}
	if cache.has(swrKey) {
		t.Fatal("a TTL-0 cache kept an entry")
	}
}
