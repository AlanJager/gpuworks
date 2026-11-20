package storagemount

import (
	"context"
	"errors"
	"sync"
	"time"
)

// ErrUnresponsive is returned when a filesystem call did not return in time, or when an earlier
// call for the same key is still blocked. Hard NFS mounts can block a syscall forever, so such a
// call is left running in the background and never retried while it is stuck.
var ErrUnresponsive = errors.New("storagemount: filesystem call did not return in time")

// guard runs functions with a time limit and at most one in flight per key.
type guard struct {
	mu sync.Mutex
	// inflight holds, per busy key, the call that holds it. A call frees the
	// key only while it still holds it, so one that returns after its key was
	// given up (abandon) does not free a later call's.
	inflight map[string]flight
	// orphans are the calls given up that have not returned yet, by number,
	// with the key they held: each keeps a goroutine.
	orphans map[uint64]string
	calls   uint64
	now     func() time.Time
}

// flight is one call in flight: its number and when it started.
type flight struct {
	call  uint64
	start time.Time
}

func newGuard(now func() time.Time) *guard {
	if now == nil {
		now = time.Now
	}
	return &guard{inflight: make(map[string]flight), orphans: make(map[uint64]string), now: now}
}

// blockedSince is when the call now holding key started, if one holds it.
func (g *guard) blockedSince(key string) (time.Time, bool) {
	g.mu.Lock()
	defer g.mu.Unlock()
	f, ok := g.inflight[key]
	return f.start, ok
}

// orphaned counts the calls given up on the keys match accepts that are
// still running.
func (g *guard) orphaned(match func(key string) bool) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for _, key := range g.orphans {
		if match(key) {
			n++
		}
	}
	return n
}

// abandon gives up the calls in flight on the keys match accepts: the keys
// are free again and those calls, if they ever return, change nothing. It is
// for calls whose filesystem was just taken away from under them (a forced
// unmount), which may never return; the caller bounds how often it does so,
// since every abandoned call that stays blocked keeps its goroutine.
func (g *guard) abandon(match func(key string) bool) int {
	g.mu.Lock()
	defer g.mu.Unlock()
	n := 0
	for key, f := range g.inflight {
		if match(key) {
			delete(g.inflight, key)
			g.orphans[f.call] = key
			n++
		}
	}
	return n
}

// run executes fn in a goroutine and waits up to timeout. On timeout, or when ctx ends first, the
// goroutine keeps running and the key stays busy until fn returns; new calls for a busy key fail
// immediately with ErrUnresponsive so stuck calls never pile up.
func (g *guard) run(ctx context.Context, key string, timeout time.Duration, fn func() error) error {
	return g.runAny(ctx, []string{key}, timeout, fn)
}

// runAny is run over a set of interchangeable keys: fn takes the first one
// that is free, and ErrUnresponsive comes back at once only when all are busy.
// It lets callers that are not stuck run side by side while still bounding how
// many calls a hung mount can hold.
func (g *guard) runAny(ctx context.Context, keys []string, timeout time.Duration, fn func() error) error {
	g.mu.Lock()
	key := ""
	for _, k := range keys {
		if _, busy := g.inflight[k]; !busy {
			key = k
			break
		}
	}
	if key == "" {
		g.mu.Unlock()
		return ErrUnresponsive
	}
	g.calls++
	call := g.calls
	g.inflight[key] = flight{call: call, start: g.now()}
	g.mu.Unlock()

	done := make(chan error, 1)
	go func() {
		err := fn()
		// Release before reporting so a caller that sees the result can immediately call again.
		g.mu.Lock()
		if g.inflight[key].call == call {
			delete(g.inflight, key)
		}
		delete(g.orphans, call)
		g.mu.Unlock()
		done <- err
	}()

	t := time.NewTimer(timeout)
	defer t.Stop()
	select {
	case err := <-done:
		return err
	case <-t.C:
		return ErrUnresponsive
	case <-ctx.Done():
		select {
		case err := <-done: // finished at the same moment: the result wins
			return err
		default:
		}
		return ctx.Err()
	}
}
