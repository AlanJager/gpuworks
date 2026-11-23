package logging

import (
	"context"
	"fmt"
	"io"
	"log/slog"
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/stallwatch"
)

const (
	// queueLines and queueBytes bound what one output may hold while it is
	// not taking writes: a few seconds of a busy server's records. Past
	// either, records for that output are dropped and counted.
	queueLines = 4096
	queueBytes = 4 << 20
	// flushTimeout bounds how long closing a logger waits for an output to
	// take what is still queued.
	flushTimeout = 3 * time.Second
	// urgentWait bounds how long a record at error level or above waits for
	// the outputs to take it before its caller goes on. On storage that
	// answers this is well under a millisecond and the record is on its way
	// out when the caller returns, so a crash right after an error does not
	// lose the error. On storage that does not answer, the first such record
	// pays this once and the ones after it pay nothing until the output has
	// caught up (queuedWriter.behind): who logs is never held for seconds.
	urgentWait = 250 * time.Millisecond
)

// queuedWriter takes records without ever waiting on its output: Write
// copies the record into a bounded queue and returns, and one goroutine
// writes the queue to the output in order.
//
// A text handler writes each record under its own lock, so a write the disk
// does not take would otherwise stop everything that logs, request handlers
// included, for as long as the disk stalls. With the queue only this
// output's goroutine waits; the process's stall watch sees that wait under
// the output's site.
//
// What is lost: records that arrive while the queue is full are dropped, and
// the next record that fits (or the next Flush or Close) is preceded by a
// line saying how many and since when. Records below error level that are
// still queued when the process dies without closing the logger (SIGKILL, a
// panic on a goroutine other than the one whose deferred closer runs) are
// lost without a trace; Close and Flush wait a bounded time for them, and a
// record at error level waits urgentWait for the output itself. The runtime
// writes a panic's own report to stderr directly, never through a queue.
type queuedWriter struct {
	out  io.Writer
	site string
	name string
	now  func() time.Time

	maxLines int
	maxBytes int

	mu      sync.Mutex
	wake    chan struct{}
	queue   [][]byte
	bytes   int   // queued and in flight
	lines   int   // queued and in flight
	pushed  int64 // records ever queued
	written int64 // records the output has taken
	dropped int64
	since   time.Time
	closed  bool
	// behind is set when the output did not take a record within
	// urgentWait and cleared when the queue is empty again.
	behind bool

	done chan struct{}
}

// newQueuedWriter starts the goroutine that writes to out. name is the
// output as the dropped-records line names it; site is the wait as a stall
// report names it.
func newQueuedWriter(out io.Writer, name, site string, maxLines, maxBytes int) *queuedWriter {
	q := &queuedWriter{out: out, site: site, name: name, now: time.Now, maxLines: maxLines, maxBytes: maxBytes,
		wake: make(chan struct{}, 1), done: make(chan struct{})}
	go q.run()
	return q
}

// Write queues a copy of p and never waits on the output. It never returns
// an error: logging must not fail the operation being logged.
func (q *queuedWriter) Write(p []byte) (int, error) {
	q.mu.Lock()
	if q.closed {
		q.mu.Unlock()
		return len(p), nil
	}
	// A record larger than the byte bound is still taken by an empty queue:
	// the bound limits what piles up, not what one record may be.
	if q.lines >= q.maxLines || (q.bytes+len(p) > q.maxBytes && q.lines > 0) {
		if q.dropped == 0 {
			q.since = q.now()
		}
		q.dropped++
		q.mu.Unlock()
		return len(p), nil
	}
	q.noteDroppedLocked()
	q.pushLocked(append([]byte(nil), p...))
	q.mu.Unlock()
	q.signal()
	return len(p), nil
}

// noteDroppedLocked queues the line that stands where dropped records would
// have been. It is not counted against the line bound, so it always fits.
func (q *queuedWriter) noteDroppedLocked() {
	if q.dropped == 0 {
		return
	}
	now := q.now()
	line := fmt.Sprintf("time=%s level=WARN msg=\"log records dropped: the output was not taking writes\" output=%s dropped=%d since=%s for=%s\n",
		now.Format(time.RFC3339Nano), q.name, q.dropped, q.since.Format(time.RFC3339Nano), now.Sub(q.since).Round(time.Millisecond))
	q.dropped = 0
	q.pushLocked([]byte(line))
}

func (q *queuedWriter) pushLocked(line []byte) {
	q.queue = append(q.queue, line)
	q.lines++
	q.pushed++
	q.bytes += len(line)
}

func (q *queuedWriter) signal() {
	select {
	case q.wake <- struct{}{}:
	default:
	}
}

// run writes the queue to the output, oldest first, until the writer is
// closed and the queue is empty.
func (q *queuedWriter) run() {
	defer close(q.done)
	for {
		q.mu.Lock()
		if len(q.queue) == 0 {
			closed := q.closed
			q.mu.Unlock()
			if closed {
				return
			}
			<-q.wake
			continue
		}
		line := q.queue[0]
		q.queue[0] = nil
		q.queue = q.queue[1:]
		q.mu.Unlock()

		end := stallwatch.Begin(q.site)
		_, _ = q.out.Write(line)
		end()

		q.mu.Lock()
		q.lines--
		q.written++
		q.bytes -= len(line)
		if q.lines == 0 {
			q.behind = false
		}
		q.mu.Unlock()
	}
}

// taken waits up to timeout for the output to take every record queued so
// far, and says whether it did. An output already known to be behind is not
// waited for.
func (q *queuedWriter) taken(timeout time.Duration) bool {
	q.mu.Lock()
	target := q.pushed
	behind := q.behind
	q.mu.Unlock()
	if behind {
		return false
	}
	deadline := time.Now().Add(timeout)
	for pause := 20 * time.Microsecond; ; {
		q.mu.Lock()
		done := q.written >= target
		if !done && !time.Now().Before(deadline) {
			q.behind = q.lines > 0
			q.mu.Unlock()
			return false
		}
		q.mu.Unlock()
		if done {
			return true
		}
		time.Sleep(pause)
		if pause < time.Millisecond {
			pause *= 2
		}
	}
}

// Close stops taking records and waits up to timeout for the output to take
// the queued ones. False means the output did not: its goroutine is still
// inside a write, and the output must not be closed under it.
func (q *queuedWriter) Close(timeout time.Duration) bool {
	q.mu.Lock()
	if !q.closed {
		q.noteDroppedLocked()
		q.closed = true
	}
	q.mu.Unlock()
	q.signal()
	timer := time.NewTimer(timeout)
	defer timer.Stop()
	select {
	case <-q.done:
		return true
	case <-timer.C:
		return false
	}
}

// live are the queues of the loggers NewLogger built and nothing closed yet.
var live struct {
	mu     sync.Mutex
	queues map[*queuedWriter]struct{}
}

func track(q *queuedWriter, on bool) {
	live.mu.Lock()
	defer live.mu.Unlock()
	if !on {
		delete(live.queues, q)
		return
	}
	if live.queues == nil {
		live.queues = map[*queuedWriter]struct{}{}
	}
	live.queues[q] = struct{}{}
}

// Flush waits, for a bounded time, until the outputs of every open logger
// have taken what is queued, the count of dropped records included. A path
// that ends or replaces the process without returning to the logger's closer
// (os.Exit, exec) calls it first, so the records that explain the exit are
// not left in memory. It reports whether everything was taken.
func Flush() bool {
	live.mu.Lock()
	queues := make([]*queuedWriter, 0, len(live.queues))
	for q := range live.queues {
		queues = append(queues, q)
	}
	live.mu.Unlock()
	for _, q := range queues {
		q.mu.Lock()
		if !q.closed {
			q.noteDroppedLocked()
		}
		q.mu.Unlock()
		q.signal()
	}
	deadline := time.Now().Add(flushTimeout)
	for _, q := range queues {
		for !q.empty() {
			if time.Now().After(deadline) {
				return false
			}
			time.Sleep(time.Millisecond)
		}
	}
	return true
}

// empty says the output has taken every record queued so far.
func (q *queuedWriter) empty() bool {
	q.mu.Lock()
	defer q.mu.Unlock()
	return q.lines == 0
}

// queuedOutput is what a logger's handler writes to: the queues while the
// logger is open and, once its closer has run, the fallback directly. A
// record logged after the closer (the default logger outlives it) is then
// written by whoever logs it instead of vanishing.
type queuedOutput struct {
	mu       sync.RWMutex
	queues   io.Writer
	fallback io.Writer
	closed   bool
}

func (o *queuedOutput) Write(p []byte) (int, error) {
	o.mu.RLock()
	defer o.mu.RUnlock()
	if o.closed {
		_, _ = o.fallback.Write(p)
		return len(p), nil
	}
	return o.queues.Write(p)
}

// close sends what is logged from now on to the fallback. It returns once no
// record is on its way into the queues any more.
func (o *queuedOutput) close() {
	o.mu.Lock()
	o.closed = true
	o.mu.Unlock()
}

// urgentHandler makes a record at error level or above wait, for at most
// urgentWait, until the outputs have taken it. The wait is outside the inner
// handler's lock, so it holds up nobody but the goroutine that logged.
type urgentHandler struct {
	slog.Handler
	queues []*queuedWriter
}

func (h urgentHandler) Handle(ctx context.Context, r slog.Record) error {
	err := h.Handler.Handle(ctx, r)
	if r.Level >= slog.LevelError {
		// One bound for all outputs: they drain side by side.
		deadline := time.Now().Add(urgentWait)
		for _, q := range h.queues {
			q.taken(time.Until(deadline))
		}
	}
	return err
}

func (h urgentHandler) WithAttrs(attrs []slog.Attr) slog.Handler {
	return urgentHandler{Handler: h.Handler.WithAttrs(attrs), queues: h.queues}
}

func (h urgentHandler) WithGroup(name string) slog.Handler {
	return urgentHandler{Handler: h.Handler.WithGroup(name), queues: h.queues}
}
