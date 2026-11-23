package logging

import (
	"fmt"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// freshUrgent gives the test the process's one wait in its initial state,
// on a clock the test moves, and puts the real one back afterwards.
func freshUrgent(t *testing.T) *clockAt {
	t.Helper()
	saved, savedBound := urgent, urgentBound
	c := &clockAt{at: time.Date(2026, 11, 23, 9, 0, 0, 0, time.UTC)}
	urgent = newUrgentState()
	urgent.now = c.now
	t.Cleanup(func() { urgent, urgentBound = saved, savedBound })
	return c
}

type clockAt struct {
	mu sync.Mutex
	at time.Time
}

func (c *clockAt) now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.at
}

func (c *clockAt) add(d time.Duration) {
	c.mu.Lock()
	c.at = c.at.Add(d)
	c.mu.Unlock()
}

// timed is an output whose writes return at once and take, by the queue's
// clock, as long as the test says.
type timed struct {
	clock *clockAt
	mu    sync.Mutex
	takes time.Duration
	lines []string
}

func (o *timed) Write(p []byte) (int, error) {
	o.mu.Lock()
	o.lines = append(o.lines, string(p))
	takes := o.takes
	o.mu.Unlock()
	o.clock.add(takes)
	return len(p), nil
}

func (o *timed) set(takes time.Duration) {
	o.mu.Lock()
	o.takes = takes
	o.mu.Unlock()
}

func (o *timed) count(substr string) int {
	o.mu.Lock()
	defer o.mu.Unlock()
	n := 0
	for _, line := range o.lines {
		if strings.Contains(line, substr) {
			n++
		}
	}
	return n
}

// urgentLogger is a logger over one queue on out, with the queue on clock.
func urgentLogger(t *testing.T, out io.Writer, clock *clockAt) (*slog.Logger, *queuedWriter) {
	t.Helper()
	q := newQueuedWriter(out, "file", StallSite, 4096, 1<<20)
	q.now = clock.now
	t.Cleanup(func() {
		if g, ok := out.(*gate); ok {
			g.release()
		}
		q.Close(5 * time.Second)
	})
	return slog.New(urgentHandler{Handler: slog.NewTextHandler(q, nil), queues: []*queuedWriter{q}}), q
}

func settled(t *testing.T, q *queuedWriter) {
	t.Helper()
	drain(t, []*queuedWriter{q})
}

// On an output that is keeping up, an error record is written when the call
// that logged it returns: a crash right after it does not take it along.
// Records below error level do not wait.
func TestAnErrorRecordIsWrittenBeforeItsCallerGoesOn(t *testing.T) {
	clock := freshUrgent(t)
	urgentBound = 10 * time.Minute // the runner's speed is not what is asserted
	path := filepath.Join(t.TempDir(), "server.log")
	log, closeLog, queues := newLogger(slog.New(slog.NewTextHandler(os.Stderr, nil)), FileOptions{Path: path, FileOnly: true})
	defer closeLog()
	for _, q := range queues {
		q.now = clock.now
	}
	for i := 0; i < 50; i++ {
		log.With("component", "test").Error("the reason the process is about to die", "n", i)
		if got := strings.Count(read(t, path), "the reason the process is about to die"); got != i+1 {
			t.Fatalf("file has %d error records when the call for record %d returned", got, i+1)
		}
	}
	if got := urgent.timeouts.Load(); got != 0 {
		t.Fatalf("%d waits ran into their bound on an output that takes writes", got)
	}
	before := urgent.waits.Load()
	for i := 0; i < 100; i++ {
		log.Warn("below error level")
	}
	if got := urgent.waits.Load(); got != before {
		t.Fatalf("%d waits for records below error level", got-before)
	}
}

// The herd: an output stops taking writes and 200 goroutines log an error
// at once. One of them waits its bound; none of the others waits, then or
// afterwards, so the stall costs the process one bound and not 200.
func TestAStuckOutputCostsOneWaitHoweverManyGoroutinesLogErrors(t *testing.T) {
	clock := freshUrgent(t)
	sink := newGate()
	log, q := urgentLogger(t, sink, clock)
	var start, done sync.WaitGroup
	start.Add(1)
	for i := 0; i < 200; i++ {
		done.Add(1)
		go func() {
			defer done.Done()
			start.Wait()
			log.Error("the disk is gone", "n", i)
		}()
	}
	start.Done()
	returns(t, "200 goroutines logging an error against a shut output", done.Wait)
	// One at most: when several are queued before the first looks, the
	// backlog already says not to wait.
	herd := urgent.waits.Load()
	if timeouts := urgent.timeouts.Load(); herd > 1 || timeouts != herd {
		t.Fatalf("%d waits and %d timeouts for 200 errors against a shut output, want one of each at most", herd, timeouts)
	}
	// Still shut: later errors, one after another, do not wait either.
	returns(t, "more errors against the shut output", func() {
		for i := 0; i < 50; i++ {
			log.Error("still gone")
		}
	})
	if waits := urgent.waits.Load(); waits != herd {
		t.Fatalf("%d more waits after the output was known not to keep up", waits-herd)
	}
	sink.release()
	settled(t, q)
	if got := strings.Count(sink.String(), "level=ERROR"); got != 250 {
		t.Fatalf("%d of the 250 error records arrived once the output took writes", got)
	}
}

// Flapping: an output that empties its queue but takes 300 ms for a write
// is not keeping up. An empty queue alone must not bring the wait back;
// only a write that was fast and left the queue empty does.
func TestAnEmptyQueueAloneDoesNotBringTheWaitBack(t *testing.T) {
	clock := freshUrgent(t)
	urgentBound = 10 * time.Minute // every write here returns; the runner's speed is not asserted
	out := &timed{clock: clock, takes: 300 * time.Millisecond}
	log, q := urgentLogger(t, out, clock)
	for i := 0; i < 20; i++ {
		log.Error("an error every 400ms", "n", i)
		settled(t, q) // the queue is empty before the next one
		clock.add(400 * time.Millisecond)
	}
	if waits := urgent.waits.Load(); waits > 1 {
		t.Fatalf("%d waits for 20 errors on an output that takes 300ms a write, want the first at most", waits)
	}
	if timeouts := urgent.timeouts.Load(); timeouts != 0 {
		t.Fatalf("%d waits ran into the bound although every write returned", timeouts)
	}
	// One fast write that leaves the queue empty: the output is keeping up
	// again, and the next error waits for it.
	out.set(0)
	log.Info("a write that is fast again")
	settled(t, q)
	before := out.count("after it recovered")
	waits := urgent.waits.Load()
	log.Error("after it recovered")
	if out.count("after it recovered") != before+1 {
		t.Fatal("the error after the output recovered was not written when its call returned")
	}
	if got := urgent.waits.Load(); got > waits+1 {
		t.Fatalf("%d waits for one error", got-waits)
	}
}

// Slow but progressing: every write returns, each after 100 ms or 240 ms.
// Ten errors in a row must not each wait for their write.
func TestASlowOutputIsWaitedForOnceAtMost(t *testing.T) {
	for _, takes := range []time.Duration{100 * time.Millisecond, 240 * time.Millisecond, fastWrite} {
		t.Run(takes.String(), func(t *testing.T) {
			clock := freshUrgent(t)
			urgentBound = 10 * time.Minute
			out := &timed{clock: clock, takes: takes}
			log, q := urgentLogger(t, out, clock)
			for i := 0; i < 10; i++ {
				log.Error("ten errors in a row", "n", i)
				settled(t, q)
			}
			if waits := urgent.waits.Load(); waits > 1 {
				t.Fatalf("%d waits for ten errors on an output that takes %s a write, want one at most", waits, takes)
			}
			if out.count("ten errors in a row") != 10 {
				t.Fatalf("%d of 10 records written", out.count("ten errors in a row"))
			}
		})
	}
}

// Backlog: an error does not wait behind a queue of other records.
func TestAnErrorDoesNotWaitBehindABacklog(t *testing.T) {
	clock := freshUrgent(t)
	sink := newGate()
	log, q := urgentLogger(t, sink, clock)
	for i := 0; i < 500; i++ {
		log.Info("ahead of the error", "n", i)
	}
	returns(t, "an error behind 500 queued records", func() { log.Error("behind the backlog") })
	if waits := urgent.waits.Load(); waits != 0 {
		t.Fatalf("%d waits behind a backlog of 500 records, want none", waits)
	}
	sink.release()
	settled(t, q)
	if !strings.HasSuffix(strings.TrimSpace(sink.String()), "msg=\"behind the backlog\"") {
		t.Fatal("the error did not arrive in its place once the output took writes")
	}
}

// The budget: whatever the outputs do, waiting stops for the rest of the
// second once 20 ms of it have been spent, and starts again with the next.
func TestWaitingIsBoundedPerSecondForTheWholeProcess(t *testing.T) {
	clock := freshUrgent(t)
	urgentBound = 10 * time.Minute
	// Each write takes 0.9 ms by the clock: fast enough to count as keeping
	// up, so every error may wait, and each wait is charged 0.9 ms.
	out := &timed{clock: clock, takes: 900 * time.Microsecond}
	log, q := urgentLogger(t, out, clock)
	for i := 0; i < 200; i++ {
		log.Error("an error storm", "n", i)
		settled(t, q)
	}
	// 200 errors took 180 ms of the clock's second. Each one that was let
	// wait was charged its 0.9 ms; they stop being let once 20 ms is spent,
	// which the 23rd reaches.
	if spent := urgent.spent; spent < urgentBudget || spent >= urgentBudget+fastWrite {
		t.Fatalf("%s of waiting charged for 200 errors in one second, want the budget of %s and no more than one write over it", spent, urgentBudget)
	}
	clock.add(time.Second)
	log.Error("the next second")
	if out.count("the next second") != 1 {
		t.Fatal("an error in the next second was not written when its call returned: the budget did not start again")
	}
	if spent := urgent.spent; spent >= urgentBudget {
		t.Fatalf("%s charged at the start of a new second", spent)
	}
}

// With a file and stderr, the output that is stuck does not stop the error
// from being on the other when the call returns, and only the stuck one is
// marked as not keeping up.
func TestAStuckOutputDoesNotCostTheOtherItsErrors(t *testing.T) {
	clock := freshUrgent(t)
	stuck, healthy := newGate(), &timed{clock: clock}
	a := newQueuedWriter(stuck, "file", StallSite, 4096, 1<<20)
	b := newQueuedWriter(healthy, "stderr", StderrStallSite, 4096, 1<<20)
	a.now, b.now = clock.now, clock.now
	t.Cleanup(func() { stuck.release(); a.Close(5 * time.Second); b.Close(5 * time.Second) })
	// Long enough that the healthy output answers inside it on any runner.
	urgentBound = 2 * time.Second
	log := slog.New(urgentHandler{Handler: slog.NewTextHandler(io.MultiWriter(a, b), nil), queues: []*queuedWriter{a, b}})
	returns(t, "an error with one output shut", func() { log.Error("first") })
	if healthy.count("first") != 1 {
		t.Fatal("the output that takes writes did not have the error when the call returned")
	}
	if a.keepingUp() || !b.keepingUp() {
		t.Fatalf("keeping up: stuck=%v healthy=%v, want only the healthy one", a.keepingUp(), b.keepingUp())
	}
	urgentBound = 10 * time.Minute // from here only the healthy output is waited for
	for i := 0; i < 20; i++ {
		log.Error("later", "n", i)
		if healthy.count("later") != i+1 {
			t.Fatalf("the healthy output had %d of %d later errors when the call returned", healthy.count("later"), i+1)
		}
	}
	if timeouts := urgent.timeouts.Load(); timeouts != 1 {
		t.Fatalf("%d waits ran into the bound, want the first alone", timeouts)
	}
}

// Flush waits for what was queued when it was called, not for the queue to
// be empty: under steady logging it still returns, and says the truth.
func TestFlushReturnsUnderSteadyLogging(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.log")
	log, closeLog, _ := newLogger(slog.New(slog.NewTextHandler(os.Stderr, nil)), FileOptions{Path: path, FileOnly: true})
	defer closeLog()
	stop, stopped := make(chan struct{}), make(chan struct{})
	go func() {
		defer close(stopped)
		for {
			select {
			case <-stop:
				return
			default:
				log.Info("steady logging")
			}
		}
	}()
	defer func() { close(stop); <-stopped }()
	log.Info("the record the flush is for")
	var flushed bool
	returns(t, "Flush under steady logging", func() { flushed = Flush() })
	if !flushed {
		t.Fatal("Flush reported records left although the output takes writes")
	}
	if !strings.Contains(read(t, path), "the record the flush is for") {
		t.Fatal("the record queued before Flush is not in the file when Flush returned")
	}
}

// An error storm from request handlers: every request logs an error. Against
// an output that takes nothing and against one that takes 100 ms for a write
// the requests are served, and the whole storm costs one wait.
func TestAnErrorStormFromRequestsCostsOneWait(t *testing.T) {
	slow := func(w io.Writer) io.Writer {
		return writerFunc(func(p []byte) (int, error) { time.Sleep(100 * time.Millisecond); return w.Write(p) })
	}
	for name, output := range map[string]func() (io.Writer, func()){
		"takes nothing":     func() (io.Writer, func()) { g := newGate(); return g, g.release },
		"100ms for a write": func() (io.Writer, func()) { return slow(io.Discard), func() {} },
	} {
		t.Run(name, func(t *testing.T) {
			saved := urgent
			urgent = newUrgentState()
			t.Cleanup(func() { urgent = saved })
			out, release := output()
			q := newQueuedWriter(out, "file", StallSite, 4096, 1<<20)
			t.Cleanup(func() { release(); q.Close(5 * time.Second) })
			log := slog.New(urgentHandler{Handler: slog.NewTextHandler(q, nil), queues: []*queuedWriter{q}})
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				log.Error("request failed", "path", r.URL.Path)
				w.WriteHeader(http.StatusInternalServerError)
			}))
			defer server.Close()
			var slowest time.Duration
			began := time.Now()
			returns(t, "1500 requests that each log an error", func() {
				for i := 0; i < 1500; i++ {
					start := time.Now()
					resp, err := http.Get(fmt.Sprintf("%s/r/%d", server.URL, i))
					if err != nil {
						t.Error(err)
						return
					}
					resp.Body.Close()
					if took := time.Since(start); took > slowest {
						slowest = took
					}
				}
			})
			t.Logf("1500 requests in %s, slowest %s, %d waits, %d timeouts", time.Since(began).Round(time.Millisecond), slowest.Round(10*time.Microsecond), urgent.waits.Load(), urgent.timeouts.Load())
			if waits := urgent.waits.Load(); waits > 1 {
				t.Fatalf("%d waits for 1500 errors, want one at most", waits)
			}
		})
	}
}

type writerFunc func([]byte) (int, error)

func (f writerFunc) Write(p []byte) (int, error) { return f(p) }

// What an error record costs its caller on an output that is keeping up,
// beside what the same record costs when the caller writes it itself.
func BenchmarkErrorRecord(b *testing.B) {
	open := func(b *testing.B) *os.File {
		f, err := os.Create(filepath.Join(b.TempDir(), "bench.log"))
		if err != nil {
			b.Fatal(err)
		}
		b.Cleanup(func() { f.Close() })
		return f
	}
	b.Run("written by the caller", func(b *testing.B) {
		log := slog.New(slog.NewTextHandler(open(b), nil))
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			log.Error("the reason the process is about to die", "n", i)
		}
	})
	b.Run("queued and waited for", func(b *testing.B) {
		saved := urgent
		urgent = newUrgentState()
		// The budget is what stops a storm; here every record is to be
		// written before its call returns, so the budget never fills.
		var tick int64
		urgent.now = func() time.Time { tick++; return time.Unix(0, tick) }
		defer func() { urgent = saved }()
		q := newQueuedWriter(open(b), "file", StallSite, 4096, 1<<20)
		defer q.Close(5 * time.Second)
		log := slog.New(urgentHandler{Handler: slog.NewTextHandler(q, nil), queues: []*queuedWriter{q}})
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			log.Error("the reason the process is about to die", "n", i)
		}
	})
	b.Run("queued and not waited for", func(b *testing.B) {
		q := newQueuedWriter(open(b), "file", StallSite, 1<<30, 1<<40)
		defer q.Close(time.Minute)
		log := slog.New(slog.NewTextHandler(q, nil))
		b.ReportAllocs()
		for i := 0; i < b.N; i++ {
			log.Error("the reason the process is about to die", "n", i)
		}
	})
}
