package monitor

import (
	"context"
	"errors"
	"sync"
	"time"
)

// CacheTTL is how long a cached snapshot stays fresh before the next caller
// recomputes it (plan §3.1's "折叠区" polling cadence and §6's implicit
// "many browsers polling the same overview" case).
const CacheTTL = 10 * time.Second

// negativeTTL bounds how long a *failed* computation's error is remembered -
// far shorter than CacheTTL: a transient failure (a tsdb hiccup, a store
// error) should deny every viewer for at most a second, not a full TTL
// window. See isContextErr's doc comment for the even shorter-lived case
// (context cancellation), which is never cached at all.
const negativeTTL = time.Second

// isContextErr reports whether err is (or wraps) a context cancellation or
// deadline. Such an error says nothing about whether the computation itself
// would have succeeded - it only means whichever caller happened to be
// leading this computation had its own request aborted - so Get never lets
// it poison the cache for anyone else: see the comment where this is used.
func isContextErr(err error) bool {
	return errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded)
}

// cacheKey identifies one cached computation: what kind of snapshot
// (cluster/service), which id (empty for cluster, the deployment id for a
// service), and which time range.
type cacheKey struct {
	kind string
	id   string
	rng  string
}

// cacheEntry holds one computation's outcome. done is closed once value/err
// are set; a caller that finds an entry whose done channel is not yet closed
// is looking at an in-flight computation and waits on it instead of starting
// a second one (single-flight).
type cacheEntry[T any] struct {
	value T
	err   error
	at    time.Time
	done  chan struct{}
}

func (e *cacheEntry[T]) closed() bool {
	select {
	case <-e.done:
		return true
	default:
		return false
	}
}

// Cached memoizes a computation per (kind, id, range) key for its TTL, and
// single-flights concurrent callers for the same key so only one computation
// runs at a time no matter how many callers ask for it simultaneously - the
// same failing or slow backend cannot be hammered by every poller. The zero
// value is not usable; construct with NewCached.
type Cached[T any] struct {
	mu      sync.Mutex
	now     func() time.Time
	ttl     time.Duration
	entries map[cacheKey]*cacheEntry[T]
}

// NewCached constructs a Cached with the given clock (nil means time.Now)
// and TTL.
func NewCached[T any](now func() time.Time, ttl time.Duration) *Cached[T] {
	if now == nil {
		now = time.Now
	}
	return &Cached[T]{now: now, ttl: ttl, entries: map[cacheKey]*cacheEntry[T]{}}
}

// Get returns the cached value for (kind, id, rng) if fresh, waits for an
// in-flight computation for the same key if one is running, or else runs
// compute itself and caches the result.
//
// compute always receives ctx exactly as this particular Get call received
// it, including when this call is the one that ends up leading the shared
// computation for every concurrent waiter. A caller (monitorapi) that shares
// one Cached across many independent HTTP requests must therefore give
// compute a context that survives any single request's own
// cancellation/timeout (context.WithoutCancel) before calling Get - otherwise
// whichever request happened to arrive first and trigger the computation
// would cancel it for everyone waiting behind it the moment that first
// request's own connection dropped. Get's own ctx parameter is still used
// for the "waiting for someone else's in-flight computation" case below, so
// an aborted individual request still stops blocking that one caller
// promptly instead of also being immune to cancellation.
//
// An error result is cached far more briefly than a success (negativeTTL,
// not ttl): a transient failure should not deny every viewer for a full TTL
// window. A context-cancellation/deadline error is not cached at all - see
// isContextErr's doc comment - because it says nothing about the
// computation's own health, only that whichever caller led it happened to
// have its request aborted; the very next caller deserves a fresh attempt,
// not a borrowed failure that has nothing to do with its own request.
//
// Every call also sweeps other entries that finished (their done channel is
// closed) and are past their own TTL (ttl for a success, negativeTTL for a
// failure) out of the map, so a key that stops being requested - a
// deployment that gets deleted, say - does not sit in the cache forever. An
// in-flight entry is never swept: it isn't expired (it has no "at" yet) and
// deleting it out from under its waiters would make each of them see a
// closed-but-never-set entry. Because the sweep runs first, under the same
// lock, a closed entry found below is guaranteed fresh - the expired case
// was already removed - so there is no separate "expired, recompute" branch
// to fall through to.
func (c *Cached[T]) Get(ctx context.Context, kind, id, rng string, compute func(ctx context.Context) (T, error)) (T, error) {
	key := cacheKey{kind: kind, id: id, rng: rng}

	c.mu.Lock()
	c.sweepExpiredLocked()
	if existing, ok := c.entries[key]; ok {
		if existing.closed() {
			c.mu.Unlock()
			return existing.value, existing.err
		}
		// In flight: wait for it, not holding the lock.
		c.mu.Unlock()
		select {
		case <-existing.done:
			return existing.value, existing.err
		case <-ctx.Done():
			var zero T
			return zero, ctx.Err()
		}
	}

	entry := &cacheEntry[T]{done: make(chan struct{})}
	c.entries[key] = entry
	c.mu.Unlock()

	value, err := compute(ctx)

	c.mu.Lock()
	entry.value, entry.err, entry.at = value, err, c.now()
	if isContextErr(err) {
		// Never let a leader's own aborted request become "the answer" for
		// this key: remove it immediately so the next caller - even one
		// arriving a nanosecond later - gets a fresh attempt instead of
		// inheriting a stranger's cancellation. Already-waiting callers
		// still hold a direct reference to entry and will see this result
		// once done closes below; that much is unavoidable (they were
		// already committed to this computation's outcome), but no *new*
		// caller can find this entry in the map again.
		delete(c.entries, key)
	}
	close(entry.done)
	c.mu.Unlock()

	return value, err
}

// sweepExpiredLocked deletes every completed, expired entry from the map.
// Callers must hold c.mu.
func (c *Cached[T]) sweepExpiredLocked() {
	for k, e := range c.entries {
		if e.closed() && c.now().Sub(e.at) >= c.ttlFor(e.err) {
			delete(c.entries, k)
		}
	}
}

// ttlFor is how long an entry with this result stays fresh: negativeTTL for
// any error (isContextErr's own immediate delete in Get makes that case
// almost never reach this path at all), c.ttl for a success.
func (c *Cached[T]) ttlFor(err error) time.Duration {
	if err != nil {
		return negativeTTL
	}
	return c.ttl
}
