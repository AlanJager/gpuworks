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
	mu       sync.Mutex
	inflight map[string]struct{}
}

func newGuard() *guard { return &guard{inflight: make(map[string]struct{})} }

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
	g.inflight[key] = struct{}{}
	g.mu.Unlock()

	done := make(chan error, 1)
	go func() {
		err := fn()
		// Release before reporting so a caller that sees the result can immediately call again.
		g.mu.Lock()
		delete(g.inflight, key)
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
