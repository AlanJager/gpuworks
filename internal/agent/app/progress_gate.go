package app

import (
	"math"
	"sync"
	"time"
)

// statusProgressInterval is the shortest gap between two progress reports for
// the same object. Progress is overwritable state: only the latest value
// matters, so intermediate frames may be dropped, but the last one may not.
const statusProgressInterval = time.Second

// statusProgressIdleExpiry drops a key that has not reported for a long time.
// Keys are normally released when the object reaches a terminal state; this is
// the backstop for an object that simply stops reporting (agent restart,
// cancelled transfer) so the table cannot grow without bound.
const statusProgressIdleExpiry = 10 * time.Minute

// statusProgressPruneAfter is the number of live keys past which report()
// sweeps for idle ones. A node tracks a handful of instances and model files,
// so the sweep normally never runs.
const statusProgressPruneAfter = 32

// gateTimer is the subset of *time.Timer the gate uses, so a test can supply a
// clock it controls instead of waiting for wall time.
type gateTimer interface{ Stop() bool }

// progressGate rate-limits an agent's outbound status reports to one per second
// per object, latest-wins.
//
// It is the single egress limiter for every kind of status the agent streams:
// image pull lines, unpack output, container-start heartbeats, health probes
// and model download/sync progress all arrive here. Limiting at the egress
// rather than at each producer is what makes the bound a property of the
// protocol instead of a property of whichever producer remembered to throttle.
//
// Three things are never delayed: the first report for an object, a report
// whose milestone (state, and phase where the status has one) differs from the
// last one, and a terminal report. Everything else is a progress sample: the
// newest suppressed one is delivered by a trailing flush once the interval
// expires, so a transfer that goes quiet still shows its final value.
type progressGate struct {
	mu       sync.Mutex
	interval time.Duration
	keys     map[string]*progressGateKey

	// now and after are injectable so the limiter's timing is testable without
	// sleeping. They default to the wall clock.
	now   func() time.Time
	after func(time.Duration, func()) gateTimer
}

type progressGateKey struct {
	lastSent  time.Time
	milestone string
	seen      bool
	pending   func()
	timer     gateTimer
	seq       uint64

	// sendMu serializes deliveries for this key and guards sentSeq. A trailing
	// flush that loses the race against a newer report is dropped rather than
	// delivered out of order — which is exactly latest-wins.
	sendMu  sync.Mutex
	sentSeq uint64
}

func newProgressGate() *progressGate {
	return &progressGate{
		interval: statusProgressInterval,
		keys:     map[string]*progressGateKey{},
		now:      time.Now,
		after: func(d time.Duration, f func()) gateTimer {
			return time.AfterFunc(d, f)
		},
	}
}

// report delivers send now, or later, or not at all, according to the rules in
// the type comment. terminal also releases the key.
func (g *progressGate) report(key, milestone string, terminal bool, send func()) {
	if send == nil {
		return
	}
	if g == nil {
		send()
		return
	}
	g.mu.Lock()
	entry := g.keys[key]
	if entry == nil {
		entry = &progressGateKey{}
		g.keys[key] = entry
	}
	now := g.now()
	immediate := !entry.seen || entry.milestone != milestone || terminal ||
		now.Sub(entry.lastSent) >= g.interval
	entry.seen = true
	entry.milestone = milestone
	entry.seq++
	seq := entry.seq
	if !immediate {
		entry.pending = send
		if entry.timer == nil {
			wait := g.interval - now.Sub(entry.lastSent)
			if wait < 0 {
				wait = 0
			}
			entry.timer = g.after(wait, func() { g.flush(entry) })
		}
		g.mu.Unlock()
		return
	}
	// The suppressed frame belongs to the phase that is ending, and it holds
	// that phase's final counters - the figures the milestone about to be
	// written summarizes. Dropping it would make every phase's summary stop
	// short of what the phase actually achieved, so it goes out first.
	trailing, trailingSeq := entry.pending, seq-1
	entry.pending = nil
	if entry.timer != nil {
		entry.timer.Stop()
		entry.timer = nil
	}
	entry.lastSent = now
	if terminal {
		delete(g.keys, key)
	} else {
		g.pruneLocked(now)
	}
	g.mu.Unlock()
	if trailing != nil {
		entry.deliver(trailingSeq, trailing)
	}
	entry.deliver(seq, send)
}

// flush delivers the newest report the interval suppressed. entry is passed in
// rather than looked up again: a key released by a terminal report has already
// dropped its pending value, so a timer that fires afterwards finds nothing.
func (g *progressGate) flush(entry *progressGateKey) {
	g.mu.Lock()
	entry.timer = nil
	send := entry.pending
	entry.pending = nil
	if send == nil {
		g.mu.Unlock()
		return
	}
	entry.lastSent = g.now()
	seq := entry.seq
	g.mu.Unlock()
	entry.deliver(seq, send)
}

func (entry *progressGateKey) deliver(seq uint64, send func()) {
	entry.sendMu.Lock()
	defer entry.sendMu.Unlock()
	if seq <= entry.sentSeq {
		// A newer report for this object has already gone out. Delivering this
		// one would move the displayed progress backwards.
		return
	}
	entry.sentSeq = seq
	send()
}

func (g *progressGate) pruneLocked(now time.Time) {
	if len(g.keys) <= statusProgressPruneAfter {
		return
	}
	for key, entry := range g.keys {
		if entry.pending != nil || entry.timer != nil {
			continue
		}
		if now.Sub(entry.lastSent) >= statusProgressIdleExpiry {
			delete(g.keys, key)
		}
	}
}

// discard forgets an object whose producer has stopped and whose unsent
// progress no longer describes anything: a suppressed sample is dropped and,
// once discard returns, no earlier report for key is delivered any more. A
// later report for key starts afresh.
func (g *progressGate) discard(key string) {
	if g == nil {
		return
	}
	g.mu.Lock()
	entry := g.keys[key]
	if entry == nil {
		g.mu.Unlock()
		return
	}
	delete(g.keys, key)
	entry.pending = nil
	if entry.timer != nil {
		entry.timer.Stop()
		entry.timer = nil
	}
	g.mu.Unlock()
	// A trailing flush may have taken the sample just before it was dropped.
	// Marking every sequence as sent makes its delivery a no-op, and holding
	// sendMu waits out one that is already being sent.
	entry.sendMu.Lock()
	entry.sentSeq = math.MaxUint64
	entry.sendMu.Unlock()
}

// tracked reports how many objects the gate currently holds state for. It
// exists so a test can assert that keys are released instead of accumulating.
func (g *progressGate) tracked() int {
	g.mu.Lock()
	defer g.mu.Unlock()
	return len(g.keys)
}
