package logging

import (
	"bytes"
	"context"
	"fmt"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/stallwatch"
)

// drain waits until every queue's output has taken what was queued.
func drain(t *testing.T, queues []*queuedWriter) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for _, q := range queues {
		for !q.empty() {
			if time.Now().After(deadline) {
				t.Fatalf("output %s still holds records", q.name)
			}
			time.Sleep(time.Millisecond)
		}
	}
}

// A path that exits or replaces the process without closing the logger
// flushes first: the file has the records when Flush returns.
func TestFlushLeavesNothingQueuedInAnOpenLogger(t *testing.T) {
	path := filepath.Join(t.TempDir(), "agent.log")
	log, closeLog := NewLogger(slog.New(slog.NewTextHandler(os.Stderr, nil)), FileOptions{Path: path, FileOnly: true})
	for i := 0; i < 300; i++ {
		log.Info("before the hand-over", "n", i)
	}
	if !Flush() {
		t.Fatal("Flush reported records left in a healthy file's queue")
	}
	if got := strings.Count(read(t, path), "before the hand-over"); got != 300 {
		t.Fatalf("file has %d of 300 records when Flush returned", got)
	}
	closeLog()
	live.mu.Lock()
	left := len(live.queues)
	live.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d queues still tracked after the logger closed", left)
	}
}

// gate is an output that takes nothing while it is shut, the way a file does
// while the disk under it stalls.
type gate struct {
	mu     sync.Mutex
	open   chan struct{}
	once   sync.Once
	buf    bytes.Buffer
	writes int
}

func newGate() *gate { return &gate{open: make(chan struct{})} }

// release lets the output take writes from now on.
func (g *gate) release() { g.once.Do(func() { close(g.open) }) }

// gatedQueue is a queue over sink that the test's end releases and closes,
// so a test that fails half way leaves no goroutine behind.
func gatedQueue(t *testing.T, sink *gate, name, site string, maxLines, maxBytes int) *queuedWriter {
	t.Helper()
	q := newQueuedWriter(sink, name, site, maxLines, maxBytes)
	t.Cleanup(func() {
		sink.release()
		q.Close(5 * time.Second)
	})
	return q
}

// returns fails the test when fn has not returned long after anything but
// a call that waits on the shut output would have. It asserts that fn is not
// held, not how fast it is: a starved runner may take seconds for anything.
func returns(t *testing.T, what string, fn func()) {
	t.Helper()
	done := make(chan struct{})
	go func() { defer close(done); fn() }()
	select {
	case <-done:
	case <-time.After(2 * time.Minute):
		t.Fatalf("%s did not return: it waits on the output", what)
	}
}

func (g *gate) Write(p []byte) (int, error) {
	<-g.open
	g.mu.Lock()
	defer g.mu.Unlock()
	g.writes++
	return g.buf.Write(p)
}

func (g *gate) String() string {
	g.mu.Lock()
	defer g.mu.Unlock()
	return g.buf.String()
}

// The defect this guards: a log file write that the disk did not take held
// the text handler's lock, and every goroutine that logged, request handlers
// included, waited behind it for seconds.
func TestAnOutputThatTakesNothingDoesNotDelayWhoLogsOrARequest(t *testing.T) {
	sink := newGate()
	q := gatedQueue(t, sink, "file", StallSite, 4096, 1<<20)
	log := slog.New(slog.NewTextHandler(q, nil))
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		log.Info("request", "path", r.URL.Path)
		w.WriteHeader(http.StatusNoContent)
	}))
	defer server.Close()

	// The output is shut for the whole loop; with the write on the caller
	// the first record would never return.
	var worst time.Duration
	started := time.Now()
	returns(t, "logging and serving requests against a shut output", func() {
		for i := 0; i < 200; i++ {
			begin := time.Now()
			log.Info("direct", "n", i)
			resp, err := http.Get(fmt.Sprintf("%s/r/%d", server.URL, i))
			if err != nil {
				t.Error(err)
				return
			}
			resp.Body.Close()
			if took := time.Since(begin); took > worst {
				worst = took
			}
		}
	})
	t.Logf("400 records and 200 requests against a shut output: %v in all, slowest pair %v", time.Since(started), worst)
	if got := sink.String(); got != "" {
		t.Fatalf("the shut output received %q", got)
	}

	sink.release()
	if !q.Close(5 * time.Second) {
		t.Fatal("the opened output did not take the queue")
	}
	// Every record arrives, each goroutine's in the order it logged them.
	lines := strings.Split(strings.TrimSuffix(sink.String(), "\n"), "\n")
	if len(lines) != 400 {
		t.Fatalf("output has %d lines, want 400", len(lines))
	}
	direct, requests := 0, 0
	for _, line := range lines {
		switch {
		case strings.Contains(line, "msg=direct"):
			if !strings.Contains(line, fmt.Sprintf(" n=%d", direct)) {
				t.Fatalf("direct record %d out of order: %s", direct, line)
			}
			direct++
		case strings.Contains(line, "msg=request"):
			if !strings.Contains(line, fmt.Sprintf("path=/r/%d", requests)) {
				t.Fatalf("request record %d out of order: %s", requests, line)
			}
			requests++
		}
	}
	if direct != 200 || requests != 200 {
		t.Fatalf("direct=%d requests=%d, want 200 each", direct, requests)
	}
}

var droppedLine = regexp.MustCompile(`^time=\S+ level=WARN msg="log records dropped: the output was not taking writes" output=file dropped=(\d+) since=(\S+) for=(\S+)$`)

func TestRecordsPastTheBoundAreDroppedCountedAndReportedInPlace(t *testing.T) {
	sink := newGate()
	q := gatedQueue(t, sink, "file", StallSite, 4, 1<<20)
	// The writing goroutine reads the clock too, to time its writes.
	clock := &clockAt{at: time.Date(2026, 11, 23, 10, 0, 0, 0, time.UTC)}
	q.now = clock.now
	write := func(s string) {
		if n, err := q.Write([]byte(s + "\n")); n != len(s)+1 || err != nil {
			t.Fatalf("Write(%q) = %d, %v", s, n, err)
		}
	}
	for i := 0; i < 4; i++ {
		write(fmt.Sprintf("kept-%d", i))
	}
	// The queue is full (one record may be in flight; it still counts).
	clock.add(time.Second)
	for i := 0; i < 7; i++ {
		write(fmt.Sprintf("lost-%d", i))
		clock.add(time.Second)
	}
	sink.release()
	drain(t, []*queuedWriter{q})
	write("after")
	if !q.Close(5 * time.Second) {
		t.Fatal("queue did not flush")
	}
	lines := strings.Split(strings.TrimSuffix(sink.String(), "\n"), "\n")
	if len(lines) != 6 || lines[0] != "kept-0" || lines[3] != "kept-3" || lines[5] != "after" {
		t.Fatalf("output = %q", lines)
	}
	m := droppedLine.FindStringSubmatch(lines[4])
	if m == nil || m[1] != "7" || m[2] != "2026-11-23T10:00:01Z" || m[3] != "7s" {
		t.Fatalf("dropped line = %q, want dropped=7 since the first loss, for=7s, where the lost records were", lines[4])
	}
	if strings.Contains(sink.String(), "lost-") {
		t.Fatal("a dropped record reached the output")
	}
}

func TestTheByteBoundDropsToo(t *testing.T) {
	sink := newGate()
	q := gatedQueue(t, sink, "file", StallSite, 1000, 64)
	_, _ = q.Write(bytes.Repeat([]byte("a"), 60))
	_, _ = q.Write(bytes.Repeat([]byte("b"), 10)) // 70 > 64
	_, _ = q.Write([]byte("cc\n"))                // fits
	sink.release()
	if !q.Close(5 * time.Second) {
		t.Fatal("queue did not flush")
	}
	got := sink.String()
	if strings.Contains(got, "bbbbbbbbbb") || !strings.Contains(got, "dropped=1") || !strings.HasSuffix(got, "cc\n") {
		t.Fatalf("output = %q", got)
	}
}

func TestRecordsLostAtTheEndAreReportedByClose(t *testing.T) {
	sink := newGate()
	q := gatedQueue(t, sink, "stderr", StderrStallSite, 1, 1<<20)
	_, _ = q.Write([]byte("kept\n"))
	_, _ = q.Write([]byte("lost\n"))
	sink.release()
	if !q.Close(5 * time.Second) {
		t.Fatal("queue did not flush")
	}
	if got := sink.String(); !strings.HasPrefix(got, "kept\n") || !strings.Contains(got, "output=stderr dropped=1") || strings.Contains(got, "lost") {
		t.Fatalf("output = %q", got)
	}
	// Closed: later records are discarded, not written and not blocking.
	if n, err := q.Write([]byte("late\n")); n != 5 || err != nil || strings.Contains(sink.String(), "late") {
		t.Fatalf("write after close = %d, %v", n, err)
	}
}

func TestCloseWaitsNoLongerThanItsBoundForAnOutputThatTakesNothing(t *testing.T) {
	sink := newGate()
	q := gatedQueue(t, sink, "file", StallSite, 16, 1<<20)
	_, _ = q.Write([]byte("stuck\n"))
	begin := time.Now()
	var flushed bool
	returns(t, "Close on an output that takes nothing", func() { flushed = q.Close(200 * time.Millisecond) })
	if flushed {
		t.Fatal("Close reported a flush the output never took")
	}
	// A timer never fires early, so the lower bound holds on any runner.
	if took := time.Since(begin); took < 200*time.Millisecond {
		t.Fatalf("Close gave up after %v, before its 200ms bound", took)
	}
}

// The logger's closer has the same bound when the file itself is what does
// not return: it must not then close the file under the stuck write.
func TestLoggerCloserIsBoundedAndFlushesWhatTheOutputsTake(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.log")
	log, closeLog, queues := newLogger(slog.New(slog.NewTextHandler(os.Stderr, nil)), FileOptions{Path: path, FileOnly: true})
	if len(queues) != 1 {
		t.Fatalf("file-only logger has %d queues, want 1", len(queues))
	}
	for i := 0; i < 500; i++ {
		log.Info("record", "n", i)
	}
	closeLog()
	lines := strings.Split(strings.TrimSuffix(read(t, path), "\n"), "\n")
	if len(lines) != 500 {
		t.Fatalf("file has %d lines after close, want all 500", len(lines))
	}
	for i, line := range lines {
		if !strings.HasSuffix(line, fmt.Sprintf(" n=%d", i)) {
			t.Fatalf("line %d out of order: %s", i, line)
		}
	}
}

func TestRotationKeepsItsBoundsBehindTheQueue(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.log")
	log, closeLog, _ := newLogger(slog.New(slog.NewTextHandler(os.Stderr, nil)), FileOptions{Path: path, MaxBytes: 2048, Keep: 3, FileOnly: true})
	for i := 0; i < 400; i++ {
		log.Info("rotating record", "n", i)
	}
	closeLog()
	// Newest generation last: the records, read oldest file first, are the
	// tail of what was logged, in order, and no file passes the cap.
	var all []string
	for _, name := range []string{path + ".3", path + ".2", path + ".1", path} {
		info, err := os.Stat(name)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if info.Size() > 2048 {
			t.Fatalf("%s is %d bytes, past the 2048 cap", name, info.Size())
		}
		all = append(all, strings.Split(strings.TrimSuffix(read(t, name), "\n"), "\n")...)
	}
	if _, err := os.Stat(path + ".4"); err == nil {
		t.Fatal("a fourth generation was kept")
	}
	first := 400 - len(all)
	for i, line := range all {
		if !strings.HasSuffix(line, fmt.Sprintf(" n=%d", first+i)) {
			t.Fatalf("record %d of the kept tail out of order: %s", i, line)
		}
	}
	if !strings.HasSuffix(all[len(all)-1], " n=399") {
		t.Fatalf("last record = %s", all[len(all)-1])
	}
}

// The stall watch sees the output's own goroutine waiting, under the
// output's site, and reports through the same logger without waiting on it.
func TestTheStallWatchReportsTheWritingGoroutineNotWhoLogs(t *testing.T) {
	dir := t.TempDir()
	sink := newGate()
	q := gatedQueue(t, sink, "file", StallSite, 4096, 1<<20)
	log := slog.New(slog.NewTextHandler(q, nil))
	watch := stallwatch.New(stallwatch.Options{Threshold: 100 * time.Millisecond, Dir: dir, Log: log})
	stallwatch.Use(watch)
	defer stallwatch.Use(nil)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); watch.Run(ctx) }()

	log.Info("the record the output does not take")
	deadline := time.Now().Add(10 * time.Second)
	for len(stallwatch.Reports(dir)) == 0 {
		if time.Now().After(deadline) {
			t.Fatal("no stall report while the output took nothing")
		}
		returns(t, "a log line during the stall", func() { log.Info("still logging") })
		time.Sleep(10 * time.Millisecond)
	}
	report := read(t, stallwatch.Reports(dir)[0])
	for _, want := range []string{StallSite, "logging.(*queuedWriter).run", "logging.(*gate).Write"} {
		if !strings.Contains(report, want) {
			t.Fatalf("report lacks %q:\n%s", want, report)
		}
	}
	sink.release()
	for !strings.Contains(sink.String(), "site=\""+StallSite+"\"") {
		if time.Now().After(deadline) {
			t.Fatalf("the stall's own log line did not arrive once the output took writes:\n%s", sink.String())
		}
		time.Sleep(10 * time.Millisecond)
	}
	cancel()
	<-done
	if !q.Close(5 * time.Second) {
		t.Fatal("queue did not flush")
	}
}

// One record larger than the byte bound is written, not dropped for ever:
// the bound limits what piles up behind an output.
func TestARecordLargerThanTheByteBoundIsTakenByAnEmptyQueue(t *testing.T) {
	sink := newGate()
	q := gatedQueue(t, sink, "file", StallSite, 1000, 64)
	big := append(bytes.Repeat([]byte("x"), 200), '\n')
	_, _ = q.Write(big)               // empty queue: taken
	_, _ = q.Write([]byte("after\n")) // behind it, over the bound: dropped
	sink.release()
	if !q.Close(5 * time.Second) {
		t.Fatal("queue did not flush")
	}
	got := sink.String()
	if !strings.HasPrefix(got, string(big)) || strings.Contains(got, "after") || !strings.Contains(got, "dropped=1") {
		t.Fatalf("output = %q", got)
	}
}

// Records dropped just before a path that exits through Flush are reported
// by Flush: no later record would come to carry the count.
func TestFlushReportsRecordsDroppedSinceTheLastOneThatFit(t *testing.T) {
	sink := newGate()
	q := gatedQueue(t, sink, "file", StallSite, 1, 1<<20)
	track(q, true)
	defer track(q, false)
	_, _ = q.Write([]byte("kept\n"))
	_, _ = q.Write([]byte("lost one\n"))
	_, _ = q.Write([]byte("lost two\n"))
	sink.release()
	if !Flush() {
		t.Fatal("Flush left records queued in an output that takes writes")
	}
	if got := sink.String(); !strings.HasPrefix(got, "kept\n") || !strings.Contains(got, "output=file dropped=2") || strings.Contains(got, "lost") {
		t.Fatalf("output when Flush returned = %q", got)
	}
}

// The default logger outlives the closer. What is logged after it goes to
// stderr, written by whoever logs, instead of vanishing.
func TestRecordsLoggedAfterTheCloserGoToStderr(t *testing.T) {
	dir := t.TempDir()
	stderr, err := os.Create(filepath.Join(dir, "stderr"))
	if err != nil {
		t.Fatal(err)
	}
	saved := os.Stderr
	os.Stderr = stderr
	t.Cleanup(func() { os.Stderr = saved; stderr.Close() })
	path := filepath.Join(dir, "server.log")
	log, closeLog, _ := newLogger(slog.New(slog.NewTextHandler(stderr, nil)), FileOptions{Path: path, FileOnly: true})
	log.Info("while open")
	closeLog()
	log.Info("after the closer")
	log.Error("an error after the closer")
	if got := read(t, path); !strings.Contains(got, "while open") || strings.Contains(got, "after the closer") {
		t.Fatalf("log file = %q", got)
	}
	if got := read(t, stderr.Name()); strings.Count(got, "after the closer") != 2 || strings.Contains(got, "while open") {
		t.Fatalf("stderr = %q, want the two records logged after the closer, once each", got)
	}
}

// A panic that unwinds the goroutine holding the closer runs the closer, so
// the records that lead up to it are written before the process reports the
// panic and dies.
func TestAPanicThatUnwindsPastTheCloserLeavesTheRecordsWritten(t *testing.T) {
	path := filepath.Join(t.TempDir(), "server.log")
	func() {
		defer func() { _ = recover() }()
		log, closeLog, _ := newLogger(slog.New(slog.NewTextHandler(os.Stderr, nil)), FileOptions{Path: path, FileOnly: true})
		defer closeLog()
		for i := 0; i < 300; i++ {
			log.Info("before the panic", "n", i)
		}
		panic("the server's main goroutine panics")
	}()
	if got := strings.Count(read(t, path), "before the panic"); got != 300 {
		t.Fatalf("file has %d of 300 records after the panic unwound", got)
	}
}
