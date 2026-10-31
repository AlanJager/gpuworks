package monitor

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"runtime/debug"
	"sync"
	"time"
)

// CacheTTL is how long a cached snapshot stays fresh. After that the next
// read still gets it, immediately, while one background computation replaces
// it (stale-while-revalidate, see Cached).
const CacheTTL = 10 * time.Second

// retainTTLs is how many TTLs a snapshot may be served after it was computed,
// and how many a key may go unread before its entry is dropped. A snapshot
// older than that is not shown any more: the reader waits for a new one.
const retainTTLs = 6

// refreshTimeout bounds one background refresh. The caller's compute may
// bound itself further; this is the backstop for one that does not.
const refreshTimeout = time.Minute

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

// cacheEntry holds one computation's outcome. done is closed once value, err
// and at are set; a caller that finds an entry whose done channel is not yet
// closed is looking at an in-flight computation and waits on it instead of
// starting a second one (single-flight). Once done is closed those three
// fields never change, so they are read without the lock; a refresh replaces
// the whole entry. The fields below them belong to Cached.mu.
type cacheEntry[T any] struct {
	value T
	err   error
	at    time.Time
	done  chan struct{}

	lastRead   time.Time // when a reader last got this entry
	refreshing bool      // a background refresh of this entry is running
	retryAt    time.Time // no new refresh before this, after one failed
}

func (e *cacheEntry[T]) closed() bool {
	select {
	case <-e.done:
		return true
	default:
		return false
	}
}

// Cached memoizes a computation per (kind, id, range) key, and single-flights
// concurrent callers for the same key so only one computation runs at a time
// no matter how many callers ask for it simultaneously - the same failing or
// slow backend cannot be hammered by every poller.
//
// A value is fresh for the TTL. Once it is older, a read still returns it at
// once and starts one background computation (at most one per key at a time)
// that replaces it, so a poller never waits for a rebuild that the poll
// before it could have started. Only a key with nothing to show makes the
// caller wait: never computed, its value too old to show (retainTTLs TTLs),
// or its last attempt a failure. A TTL of 0 keeps nothing: every read
// computes. The zero value is not usable; construct with NewCached.
type Cached[T any] struct {
	mu        sync.Mutex
	now       func() time.Time
	ttl       time.Duration
	entries   map[cacheKey]*cacheEntry[T]
	lastSweep time.Time
}

// NewCached constructs a Cached with the given clock (nil means time.Now)
// and TTL.
func NewCached[T any](now func() time.Time, ttl time.Duration) *Cached[T] {
	if now == nil {
		now = time.Now
	}
	return &Cached[T]{now: now, ttl: ttl, entries: map[cacheKey]*cacheEntry[T]{}}
}

// Get is GetAt without the time the value was computed.
func (c *Cached[T]) Get(ctx context.Context, kind, id, rng string, compute func(ctx context.Context) (T, error)) (T, error) {
	value, _, err := c.GetAt(ctx, kind, id, rng, compute)
	return value, err
}

// GetAt returns the cached value for (kind, id, rng) and when it was
// computed. A stale value is returned as it is, with the time it was
// computed: its owner, not the cache, knows what in it ages (a response's own
// as_of is the newest sample it was built from and stays what it was), and
// the time tells the caller how old what it serves is.
//
// With nothing to serve it waits for an in-flight computation of the same
// key if one is running, or else runs compute itself and caches the result.
//
// compute always receives ctx exactly as this particular GetAt call received
// it, including when this call is the one that ends up leading the shared
// computation for every concurrent waiter. A caller (monitorapi) that shares
// one Cached across many independent HTTP requests must therefore give
// compute a context that survives any single request's own
// cancellation/timeout (context.WithoutCancel) before calling GetAt -
// otherwise whichever request happened to arrive first and trigger the
// computation would cancel it for everyone waiting behind it the moment that
// first request's own connection dropped. GetAt's own ctx parameter is still
// used for the "waiting for someone else's in-flight computation" case below,
// so an aborted individual request still stops blocking that one caller
// promptly instead of also being immune to cancellation. A background refresh
// does not use ctx at all: it runs under its own context with a timeout
// (refreshTimeout) and hands that to the compute of the call that started it,
// so compute must not depend on anything of that call's request.
//
// An error result is cached far more briefly than a success (negativeTTL,
// not ttl): a transient failure should not deny every viewer for a full TTL
// window. A context-cancellation/deadline error is not cached at all - see
// isContextErr's doc comment - because it says nothing about the
// computation's own health, only that whichever caller led it happened to
// have its request aborted; the very next caller deserves a fresh attempt,
// not a borrowed failure that has nothing to do with its own request. A
// failed background refresh is not shown at all: the previous value stays and
// a read after negativeTTL tries again.
//
// Every call also sweeps the entries that can no longer be served out of the
// map (see expiredLocked), so a key that stops being requested - a
// deployment that gets deleted, say - does not sit in the cache forever. An
// in-flight entry is never swept: it has no "at" yet, and deleting it out
// from under its waiters would make each of them see a closed-but-never-set
// entry.
func (c *Cached[T]) GetAt(ctx context.Context, kind, id, rng string, compute func(ctx context.Context) (T, error)) (T, time.Time, error) {
	key := cacheKey{kind: kind, id: id, rng: rng}

	c.mu.Lock()
	now := c.now()
	c.sweepLocked(now)
	if existing, ok := c.entries[key]; ok {
		if !existing.closed() {
			// In flight: wait for it, not holding the lock.
			c.mu.Unlock()
			select {
			case <-existing.done:
				return existing.value, existing.at, existing.err
			case <-ctx.Done():
				var zero T
				return zero, time.Time{}, ctx.Err()
			}
		}
		if !c.expiredLocked(existing, now) {
			existing.lastRead = now
			if existing.err == nil && now.Sub(existing.at) >= c.ttl {
				c.refreshLocked(key, existing, compute, now)
			}
			c.mu.Unlock()
			return existing.value, existing.at, existing.err
		}
		// Nothing to serve from it any more: compute below, as for a new key.
	}

	entry := &cacheEntry[T]{done: make(chan struct{})}
	c.entries[key] = entry
	c.mu.Unlock()

	value, err := compute(ctx)

	c.mu.Lock()
	entry.value, entry.err, entry.at = value, err, c.now()
	entry.lastRead = entry.at
	if isContextErr(err) {
		// Never let a leader's own aborted request become "the answer" for
		// this key: remove it immediately so the next caller - even one
		// arriving a nanosecond later - gets a fresh attempt instead of
		// inheriting a stranger's cancellation. Already-waiting callers
		// still hold a direct reference to entry and will see this result
		// once done closes below; that much is unavoidable (they were
		// already committed to this computation's outcome), but no *new*
		// caller can find this entry in the map again.
		if c.entries[key] == entry {
			delete(c.entries, key)
		}
	}
	close(entry.done)
	c.mu.Unlock()

	return value, entry.at, err
}

// refreshLocked starts the background computation that replaces entry, unless
// one is already running for it or a failed one asked to wait. Callers must
// hold c.mu.
func (c *Cached[T]) refreshLocked(key cacheKey, entry *cacheEntry[T], compute func(ctx context.Context) (T, error), now time.Time) {
	if entry.refreshing || now.Before(entry.retryAt) {
		return
	}
	entry.refreshing = true
	go c.refresh(key, entry, compute)
}

// refresh computes a new value for key under a context of its own (no
// request's) and swaps it in for old. A failure, a context error or a panic
// leaves old in place, to be retried by a read after negativeTTL; so does the
// entry having been dropped or replaced meanwhile, whose result is then not
// wanted.
func (c *Cached[T]) refresh(key cacheKey, old *cacheEntry[T], compute func(ctx context.Context) (T, error)) {
	ctx, cancel := context.WithTimeout(context.Background(), refreshTimeout)
	defer cancel()
	value, err := guardedCompute(ctx, key, compute)

	c.mu.Lock()
	defer c.mu.Unlock()
	if c.entries[key] != old {
		return
	}
	now := c.now()
	if err != nil {
		old.refreshing = false
		old.retryAt = now.Add(negativeTTL)
		return
	}
	done := make(chan struct{})
	close(done)
	c.entries[key] = &cacheEntry[T]{value: value, at: now, done: done, lastRead: old.lastRead}
}

// guardedCompute runs compute, turning a panic into an error: a background
// goroutine has no request handler above it to recover, so a panic in it
// would take the whole server down.
func guardedCompute[T any](ctx context.Context, key cacheKey, compute func(ctx context.Context) (T, error)) (value T, err error) {
	defer func() {
		if r := recover(); r != nil {
			slog.Error("monitor cache refresh panicked", "kind", key.kind, "id", key.id, "range", key.rng, "panic", r, "stack", string(debug.Stack()))
			err = fmt.Errorf("monitor cache refresh panicked: %v", r)
		}
	}()
	value, err = compute(ctx)
	if err != nil {
		slog.Warn("monitor cache refresh failed; serving the previous value", "kind", key.kind, "id", key.id, "range", key.rng, "err", err)
	}
	return value, err
}

// sweepLocked deletes the entries that can no longer be served, at most once
// per TTL (the lookup of the key being read checks its own entry anyway).
// Callers must hold c.mu.
func (c *Cached[T]) sweepLocked(now time.Time) {
	if now.Sub(c.lastSweep) < c.ttl {
		return
	}
	c.lastSweep = now
	for k, e := range c.entries {
		if c.expiredLocked(e, now) {
			delete(c.entries, k)
		}
	}
}

// expiredLocked reports whether a finished entry has nothing left to serve:
// a failure after negativeTTL, a success older than retainTTLs TTLs, or any
// entry nobody read for that long. An in-flight entry is never expired.
// Callers must hold c.mu.
func (c *Cached[T]) expiredLocked(e *cacheEntry[T], now time.Time) bool {
	if !e.closed() {
		return false
	}
	if e.err != nil {
		return now.Sub(e.at) >= negativeTTL
	}
	retain := retainTTLs * c.ttl
	return now.Sub(e.at) >= retain || now.Sub(e.lastRead) >= retain
}
