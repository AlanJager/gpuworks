package logging

import (
	"context"
	"log/slog"
	"sync/atomic"
	"time"
)

// A record at error level or above is often the last thing a process says
// before it dies, and a queued record that the output has not written yet
// dies with it. So such a record may wait for the outputs to take it — but
// only on terms under which logging cannot hold up the work that logs:
//
//   - One wait at a time in the whole process. A record that finds another
//     one waiting does not wait.
//   - Only for an output that is keeping up: its last write took less than
//     fastWrite (1 ms) and at most urgentBacklog records are ahead. An output
//     whose write was slower, or that did not take a waiting record in time,
//     is not waited for again until a write has been fast and left its queue
//     empty; an empty queue alone does not count.
//   - For at most urgentBound (20 ms): a write to an output that is keeping
//     up is microseconds, and 20 ms covers a pause of the writing goroutine
//     while staying below what a request would notice.
//   - For at most urgentBudget (20 ms) of waiting in each urgentWindow (1 s),
//     all goroutines together.
//
// What logging can therefore cost its callers, whatever the outputs do: one
// call at most 20 ms, and all calls of the process together less than 40 ms
// in any accounting second (a wait that begins just inside the budget may
// run its full bound). In particular:
//
//   - an output that takes nothing: one wait of 20 ms, then none until it has
//     recovered;
//   - an output whose every write is slow (1 ms or more): the first error
//     waits for that write, 20 ms at most, and no later one waits;
//   - a storm of errors on an output that is keeping up: the time of one
//     write and a hand-over to the writing goroutine and back for each
//     record that is waited for, one goroutine at a time, 20 ms per second
//     in all; the rest return at once.
//
// Two things that sum does not cover:
//
//   - The budget is kept per accounting second, not over a sliding one: a
//     wait at the end of one second and the waits of the next can put close
//     to 80 ms of waiting into some one-second span.
//   - It counts the goroutine that waits. A caller that logs an error while
//     holding a mutex of its own holds that mutex for up to 20 ms longer,
//     and whoever is blocked on that mutex meanwhile is delayed too, outside
//     this accounting. Callers were not audited for logging under a lock.
//
// What is given up: an error record that did not wait (another was waiting,
// the output was not keeping up, or the budget was spent) is queued like any
// other record, and a crash in the moments after it can lose it.
const (
	urgentBacklog = 4
	urgentBudget  = 20 * time.Millisecond
	urgentWindow  = time.Second
)

// urgentBound is a variable so that a test on a starved machine can take
// the time limit out of what it asserts.
var urgentBound = 20 * time.Millisecond

// urgentState is the one wait the process allows. Everything but slot and
// the two counters belongs to the goroutine that holds the slot.
type urgentState struct {
	slot atomic.Bool

	now         func() time.Time
	windowStart time.Time
	spent       time.Duration
	// taken gets one token from each output that has taken the record
	// being waited for.
	taken chan struct{}
	timer *time.Timer

	// waits and timeouts count the waits begun and the ones that ran into
	// urgentBound.
	waits, timeouts atomic.Int64
}

var urgent = newUrgentState()

func newUrgentState() *urgentState {
	return &urgentState{now: time.Now, taken: make(chan struct{}, 8)}
}

// urgentHandler makes a record at error level or above wait for the outputs
// on the terms above. The wait is outside the inner handler's lock.
type urgentHandler struct {
	slog.Handler
	queues []*queuedWriter
}

func (h urgentHandler) Handle(ctx context.Context, r slog.Record) error {
	if r.Level < slog.LevelError || len(h.queues) > cap(urgent.taken) || !urgent.slot.CompareAndSwap(false, true) {
		return h.Handler.Handle(ctx, r)
	}
	defer urgent.slot.Store(false)
	began := urgent.now()
	if !urgent.withinBudget(began) {
		return h.Handler.Handle(ctx, r)
	}
	// Which outputs are keeping up is decided before the record is queued:
	// afterwards its own place in the queue would count against it.
	var ready [8]*queuedWriter
	n := 0
	for _, q := range h.queues {
		if q.keepingUp() {
			ready[n] = q
			n++
		}
	}
	err := h.Handler.Handle(ctx, r)
	if n > 0 {
		urgent.wait(ready[:n])
		urgent.spent += urgent.now().Sub(began)
	}
	return err
}

func (h urgentHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return urgentHandler{Handler: h.Handler.WithAttrs(attrs), queues: h.queues}
}

func (h urgentHandler) WithGroup(name string) slog.Handler {
	return urgentHandler{Handler: h.Handler.WithGroup(name), queues: h.queues}
}

// withinBudget says whether this accounting second still has waiting time.
func (u *urgentState) withinBudget(now time.Time) bool {
	if now.Sub(u.windowStart) >= urgentWindow {
		u.windowStart = now
		u.spent = 0
	}
	return u.spent < urgentBudget
}

// wait returns when every output in ready has taken what is queued for it,
// or after urgentBound. The outputs write side by side, so each has had the
// whole bound when it runs out; the ones that have not taken the record by
// then are not waited for again until they have recovered.
func (u *urgentState) wait(ready []*queuedWriter) {
	// A token an output sent while the previous wait was giving up.
	for len(u.taken) > 0 {
		<-u.taken
	}
	pending := 0
	for _, q := range ready {
		if q.expect(u.taken) {
			pending++
		}
	}
	if pending == 0 {
		return
	}
	u.waits.Add(1)
	if u.timer == nil {
		u.timer = time.NewTimer(urgentBound)
	} else {
		u.timer.Reset(urgentBound)
	}
	defer u.timer.Stop()
	for pending > 0 {
		select {
		case <-u.taken:
			pending--
		case <-u.timer.C:
			u.timeouts.Add(1)
			for _, q := range ready {
				q.giveUp()
			}
			return
		}
	}
}

// keepingUp says whether an error record may wait for this output.
func (q *queuedWriter) keepingUp() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return !q.closed && q.fast && q.lines <= urgentBacklog
}

// expect asks the output's goroutine for a token on taken once it has
// written everything queued so far. False means it already has.
func (q *queuedWriter) expect(taken chan<- struct{}) bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.written >= q.pushed {
		return false
	}
	q.urgent, q.urgentTarget = taken, q.pushed
	return true
}

// giveUp withdraws what expect asked for. An output that had not answered
// did not take the record within the bound: it is not keeping up.
func (q *queuedWriter) giveUp() {
	q.mu.Lock()
	defer q.mu.Unlock()
	if q.urgent != nil {
		q.urgent = nil
		q.fast = false
	}
}
