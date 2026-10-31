package cardsmoke

import (
	"context"
	"sync"
	"time"
)

// Which cards a check is on right now, or was on a moment ago.
//
// A check is a process holding the card for its duration: a context, a small
// allocation, for a container also its start. Two other things on the node
// look at the same card meanwhile and must not mistake it for something else:
//
//   - The occupancy report lists the processes on each card. One that reads
//     the list while the check runs sees a process on a card no instance holds,
//     and would report it as used outside the platform (Touched).
//   - The engine starting an instance on the card. On a card its runtime lets
//     only one process open at a time (an Ascend die without device-share), the
//     engine's start and the check would fail each other (WaitIdle).
//
// An attempt counts from before the process starts until it has returned, so
// one abandoned at the deadline and still stuck in the driver stays in flight.

// attemptLog is the in-flight and recently ended attempts by card.
type attemptLog struct {
	mu       sync.Mutex
	inFlight map[string]int
	ended    map[string]time.Time
}

// Begin marks an attempt on the card as started and returns the function that
// marks it ended. Doctor's checks go through it too, so they are seen the same
// way as the periodic ones.
func (r *Runner) Begin(ident string) (end func()) {
	if r == nil {
		return func() {}
	}
	l := &r.attempts
	l.mu.Lock()
	if l.inFlight == nil {
		l.inFlight, l.ended = map[string]int{}, map[string]time.Time{}
	}
	l.inFlight[ident]++
	l.mu.Unlock()
	var once sync.Once
	return func() {
		once.Do(func() {
			l.mu.Lock()
			if l.inFlight[ident]--; l.inFlight[ident] <= 0 {
				delete(l.inFlight, ident)
			}
			l.ended[ident] = r.cfg.Now()
			l.mu.Unlock()
		})
	}
}

// Touched reports whether a check is on the card now, or ended within grace.
// What the vendor tool listed a few seconds ago (npu-smi's table is reused
// for five) can still show the check's process.
func (r *Runner) Touched(ident string, grace time.Duration) bool {
	if r == nil {
		return false
	}
	l := &r.attempts
	l.mu.Lock()
	defer l.mu.Unlock()
	if l.inFlight[ident] > 0 {
		return true
	}
	ended, ok := l.ended[ident]
	return ok && r.cfg.Now().Sub(ended) < grace
}

// WaitIdle waits until no check is on any of the cards, for at most limit,
// and reports whether they became free. A check takes well under a second on
// a healthy card; one stuck in the driver is not waited out.
func (r *Runner) WaitIdle(ctx context.Context, idents []string, limit time.Duration) bool {
	if r == nil || len(idents) == 0 {
		return true
	}
	deadline := time.Now().Add(limit)
	for {
		busy := false
		r.attempts.mu.Lock()
		for _, ident := range idents {
			busy = busy || r.attempts.inFlight[ident] > 0
		}
		r.attempts.mu.Unlock()
		if !busy {
			return true
		}
		if time.Now().After(deadline) {
			return false
		}
		select {
		case <-ctx.Done():
			return false
		case <-time.After(50 * time.Millisecond):
		}
	}
}
