// Package holdpass runs the passes of the scheduling holds that a module
// asks for when its answers change (store.ApplySchedulingHolds, or
// store.ApplySchedulingHoldsFor when it names the nodes whose answers
// changed), rather than leaving each node to its next status report.
//
// A pass is one writer transaction over the fleet, or over the nodes the
// requests named, so requests are coalesced: any number of them while a
// pass is pending or running make at most one more pass, and passes are at
// least a floor apart, so a holder whose answers flap costs one pass per
// floor, not one per change. A failed
// pass is retried with a backoff that starts at the floor and doubles up to
// MaxRetry.
package holdpass

import (
	"context"
	"log/slog"
	"slices"
	"sync"
	"time"
)

// MaxRetry bounds the wait before a failed pass is retried.
const MaxRetry = time.Minute

// Apply is one pass over every node: it returns the nodes it held and those
// it released.
type Apply func() (held, released []string, err error)

// ScopedApply is one pass over nodes, ascending node ids, or over every node
// when nodes is nil: it returns the nodes it held and those it released.
type ScopedApply func(nodes []uint) (held, released []string, err error)

// MaxScoped is the most nodes one scoped pass names: requests that name more
// between two passes make a pass over every node instead, which costs about
// the same and keeps the pending set bounded.
const MaxScoped = 512

// Runner coalesces pass requests and runs them on its own loop.
type Runner struct {
	apply ScopedApply
	log   *slog.Logger
	floor time.Duration
	wake  chan struct{}

	// mu guards the scope pending for the next pass: every node, or the
	// nodes requested since the last pass took its scope.
	mu    sync.Mutex
	all   bool
	nodes map[uint]struct{}
}

// New is a runner of apply whose passes are at least floor apart; every
// pass is over every node.
func New(apply Apply, log *slog.Logger, floor time.Duration) *Runner {
	return NewScoped(func([]uint) ([]string, []string, error) { return apply() }, log, floor)
}

// NewScoped is a runner of apply whose passes are at least floor apart and
// cover only the nodes requested, unless a request asked for every node.
func NewScoped(apply ScopedApply, log *slog.Logger, floor time.Duration) *Runner {
	if log == nil {
		log = slog.Default()
	}
	return &Runner{apply: apply, log: log, floor: floor, wake: make(chan struct{}, 1)}
}

// Request asks for a pass over every node. It returns at once and allocates
// nothing: a request made while one is already pending joins it. A pass that
// starts after the request reads every holder's answer as it stands then.
// Without Run, or after it returned, the request waits for the next Run.
func (r *Runner) Request() {
	r.mu.Lock()
	r.all, r.nodes = true, nil
	r.mu.Unlock()
	r.signal()
}

// RequestNodes asks for a pass over nodes, the nodes whose answers changed;
// it joins a pending request as Request does, and with no nodes it does
// nothing.
func (r *Runner) RequestNodes(nodes []uint) {
	if len(nodes) == 0 {
		return
	}
	r.mu.Lock()
	r.addLocked(nodes)
	r.mu.Unlock()
	r.signal()
}

func (r *Runner) addLocked(nodes []uint) {
	if r.all {
		return
	}
	if r.nodes == nil {
		r.nodes = make(map[uint]struct{}, len(nodes))
	}
	for _, id := range nodes {
		r.nodes[id] = struct{}{}
	}
	if len(r.nodes) > MaxScoped {
		r.all, r.nodes = true, nil
	}
}

func (r *Runner) signal() {
	select {
	case r.wake <- struct{}{}:
	default:
	}
}

// take hands the pending scope to a pass: nil for every node. ok is false
// when nothing is pending (an earlier pass already took it).
func (r *Runner) take() (nodes []uint, ok bool) {
	r.mu.Lock()
	defer r.mu.Unlock()
	switch {
	case r.all:
		r.all = false
		return nil, true
	case len(r.nodes) == 0:
		return nil, false
	}
	nodes = make([]uint, 0, len(r.nodes))
	for id := range r.nodes {
		nodes = append(nodes, id)
	}
	r.nodes = nil
	slices.Sort(nodes)
	return nodes, true
}

// putBack returns the scope of a failed pass to the pending one, so its
// retry covers it.
func (r *Runner) putBack(nodes []uint) {
	r.mu.Lock()
	defer r.mu.Unlock()
	if nodes == nil {
		r.all, r.nodes = true, nil
		return
	}
	r.addLocked(nodes)
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
			r.signal() // the pass is still owed: the next Run makes it
			return
		}
		// Requests up to here are served by this pass; one made while it
		// runs leaves the wake set, so another pass follows the floor.
		select {
		case <-r.wake:
		default:
		}
		nodes, ok := r.take()
		if !ok {
			retry = 0
			continue
		}
		last = time.Now()
		held, released, err := r.apply(nodes)
		switch {
		case err != nil:
			r.putBack(nodes)
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
