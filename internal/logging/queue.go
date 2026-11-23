package logging

import (
	"fmt"
	"io"
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
// the next record that fits is preceded by a line saying how many and since
// when. Records still queued when the process dies without closing the
// logger (a crash, SIGKILL) are lost without a trace; Close waits a bounded
// time for them.
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
	bytes   int // queued and in flight
	lines   int // queued and in flight
	dropped int64
	since   time.Time
	closed  bool

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
	if q.lines >= q.maxLines || q.bytes+len(p) > q.maxBytes {
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
		q.bytes -= len(line)
		q.mu.Unlock()
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
// have taken what is queued. A path that ends or replaces the process without
// returning to the logger's closer (os.Exit, exec) calls it first, so the
// records that explain the exit are not left in memory. It reports whether
// everything was taken.
func Flush() bool {
	live.mu.Lock()
	queues := make([]*queuedWriter, 0, len(live.queues))
	for q := range live.queues {
		queues = append(queues, q)
	}
	live.mu.Unlock()
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
