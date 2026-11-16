// Package holdpass runs the fleet passes of the scheduling holds that a
// module asks for when its answers change (store.ApplySchedulingHolds),
// rather than leaving each node to its next status report.
//
// A pass is one writer transaction over the fleet, so requests are
// coalesced: any number of them while a pass is pending or running make at
// most one more pass, and passes are at least a floor apart, so a holder
// whose answers flap costs one pass per floor, not one per change. A failed
// pass is retried with a backoff that starts at the floor and doubles up to
// MaxRetry.
package holdpass

import (
	"context"
	"log/slog"
	"time"
)

// MaxRetry bounds the wait before a failed pass is retried.
const MaxRetry = time.Minute

// Apply is one pass: it returns the nodes it held and those it released.
type Apply func() (held, released []string, err error)

// Runner coalesces pass requests and runs them on its own loop.
type Runner struct {
	apply Apply
	log   *slog.Logger
	floor time.Duration
	wake  chan struct{}
}

// New is a runner of apply whose passes are at least floor apart.
func New(apply Apply, log *slog.Logger, floor time.Duration) *Runner {
	if log == nil {
		log = slog.Default()
	}
	return &Runner{apply: apply, log: log, floor: floor, wake: make(chan struct{}, 1)}
}

// Request asks for a pass. It never blocks and allocates nothing: a request
// made while one is already pending joins it. A pass that starts after the
// request reads every holder's answer as it stands then. Without Run, or
// after it returned, the request waits for the next Run.
func (r *Runner) Request() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// Run runs requested passes until ctx is done. It may be called again after
// it returned; a request left pending then is served by the next Run.
func (r *Runner) Run(ctx context.Context) {
	var last time.Time
	retry := time.Duration(0)
	for {
		if retry == 0 {
			select {
			case <-ctx.Done():
				return
			case <-r.wake:
			}
		}
		wait := r.floor - time.Since(last)
		if retry > 0 {
			wait = retry
		}
		if !last.IsZero() && wait > 0 && !sleep(ctx, wait) {
			r.Request() // the pass is still owed: the next Run makes it
			return
		}
		// Requests up to here are served by this pass; one made while it
		// runs leaves the wake set, so another pass follows the floor.
		select {
		case <-r.wake:
		default:
		}
		last = time.Now()
		held, released, err := r.apply()
		switch {
		case err != nil:
			retry = max(r.floor, min(2*retry, MaxRetry))
			r.log.Error("apply scheduling holds", "err", err, "retry_in", retry)
		case len(held) > 0 || len(released) > 0:
			retry = 0
			r.log.Info("applied scheduling holds", "held", len(held), "released", len(released))
		default:
			retry = 0
			r.log.Debug("applied scheduling holds: no node changed")
		}
	}
}

func sleep(ctx context.Context, d time.Duration) bool {
	t := time.NewTimer(d)
	defer t.Stop()
	select {
	case <-ctx.Done():
		return false
	case <-t.C:
		return true
	}
}
