package stallwatch

import (
	"bytes"
	"context"
	"log/slog"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

// blockedSink is a log destination whose writes wait until released, as a
// disk that stopped answering makes them.
type blockedSink struct {
	release chan struct{}
	mu      sync.Mutex
	buf     bytes.Buffer
}

func (s *blockedSink) Write(p []byte) (int, error) {
	<-s.release
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.Write(p)
}

func (s *blockedSink) text() string {
	s.mu.Lock()
	defer s.mu.Unlock()
	return s.buf.String()
}

func waitFor(t *testing.T, what string, ok func() bool) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for !ok() {
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for %s", what)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func readReports(t *testing.T, dir string) string {
	t.Helper()
	var all strings.Builder
	for _, path := range Reports(dir) {
		raw, err := os.ReadFile(path)
		if err != nil {
			t.Fatal(err)
		}
		all.Write(raw)
	}
	return all.String()
}

// The logger is what stalls, and the watchdog reports through that same
// logger: the report must still reach the disk while the logger is blocked,
// with the blocked write's stack in it, and the log line follows once the
// logger answers again.
func TestBlockedLogWriteLeavesAReportWhileTheLoggerIsStillBlocked(t *testing.T) {
	dir := filepath.Join(t.TempDir(), "stalls")
	sink := &blockedSink{release: make(chan struct{})}
	log := slog.New(slog.NewTextHandler(Writer("log output", sink), nil))
	w := New(Options{Threshold: 100 * time.Millisecond, Dir: dir, Log: log, Header: []string{"program: test 1.2.3"}})
	Use(w)
	t.Cleanup(func() { Use(nil) })
	ctx, cancel := context.WithCancel(context.Background())
	stopped := make(chan struct{})
	go func() { defer close(stopped); w.Run(ctx) }()

	wrote := make(chan struct{})
	go func() { defer close(wrote); log.Info("a record the disk does not take") }()

	waitFor(t, "the report", func() bool { return strings.Contains(readReports(t, dir), "goroutines:") })
	report := readReports(t, dir)
	for _, want := range []string{
		"gpuworks stall report", "program: test 1.2.3", "threshold: 100ms", "log output",
		"stallwatch.(*blockedSink).Write", "stallwatch.watchedWriter.Write",
	} {
		if !strings.Contains(report, want) {
			t.Errorf("report lacks %q:\n%s", want, report)
		}
	}
	if strings.Contains(report, "ended:") {
		t.Errorf("report says the stall ended while the write is still blocked:\n%s", report)
	}
	select {
	case <-wrote:
		t.Fatal("the blocked write returned before it was released")
	default:
	}

	close(sink.release)
	<-wrote
	waitFor(t, "the end of the stall", func() bool { return strings.Contains(readReports(t, dir), "ended:") })
	waitFor(t, "the log line", func() bool { return strings.Contains(sink.text(), "process stall recorded") })
	line := sink.text()
	if !strings.Contains(line, "site=\"log output\"") || !strings.Contains(line, "report="+Reports(dir)[0]) || !strings.Contains(line, "duration=") {
		t.Errorf("log line lacks the site, the duration or the report path: %s", line)
	}
	if n := strings.Count(line, "process stall recorded"); n != 1 {
		t.Errorf("stall logged %d times, want once", n)
	}
	cancel()
	<-stopped
}

// clock is a hand-moved time source for the watch's own decisions.
type clock struct{ at time.Time }

func (c *clock) now() time.Time      { return c.at }
func (c *clock) add(d time.Duration) { c.at = c.at.Add(d) }
func fixedStacks(text string) func() ([]byte, bool) {
	return func() ([]byte, bool) { return []byte(text), false }
}

func newManual(t *testing.T, opts Options) (*Watchdog, *clock, *bytes.Buffer) {
	t.Helper()
	c := &clock{at: time.Date(2026, 11, 23, 3, 0, 0, 0, time.UTC)}
	var logged bytes.Buffer
	opts.now = c.now
	if opts.stacks == nil {
		opts.stacks = fixedStacks("goroutine 1 [running]:\n")
	}
	opts.Log = slog.New(slog.NewTextHandler(&logged, nil))
	return New(opts), c, &logged
}

// run moves the clock by d the way the ticker would, one look per interval,
// and returns what the looks found.
func run(w *Watchdog, c *clock, d time.Duration) []event {
	var events []event
	for end := c.at.Add(d); c.at.Before(end); {
		c.add(w.interval())
		events = append(events, step(w, c)...)
	}
	return events
}

// step is one look, with what it found written out as Run would.
func step(w *Watchdog, c *clock) []event {
	events := w.check(c.now())
	for _, ev := range events {
		w.record(ev)
	}
	return events
}

func TestWaitsUnderTheThresholdLeaveNothing(t *testing.T) {
	dir := t.TempDir()
	w, c, logged := newManual(t, Options{Threshold: 3 * time.Second, Dir: dir})
	end := w.Begin("database statement")
	if events := run(w, c, 2250*time.Millisecond); len(events) != 0 {
		t.Fatalf("events for a wait under the threshold: %+v", events)
	}
	end()
	run(w, c, time.Second)
	if len(Reports(dir)) != 0 || logged.Len() != 0 {
		t.Fatalf("reports %v, log %q; want none", Reports(dir), logged.String())
	}
}

func TestStallsCloseTogetherShareOneReportAndTheNextCountsThem(t *testing.T) {
	dir := t.TempDir()
	w, c, logged := newManual(t, Options{Threshold: 3 * time.Second, Dir: dir, MinInterval: time.Minute})
	stall := func() {
		end := w.Begin("log output")
		run(w, c, 4*time.Second)
		end()
		run(w, c, time.Second)
	}
	stall()
	stall()
	stall()
	if got := len(Reports(dir)); got != 1 {
		t.Fatalf("%d reports for three stalls inside one interval, want 1", got)
	}
	if n := strings.Count(logged.String(), "process stall recorded"); n != 1 {
		t.Fatalf("%d log lines for three stalls inside one interval, want 1:\n%s", n, logged.String())
	}
	run(w, c, time.Minute)
	stall()
	reports := Reports(dir)
	if len(reports) != 2 {
		t.Fatalf("%d reports after the interval passed, want 2", len(reports))
	}
	raw, _ := os.ReadFile(reports[1])
	if !strings.Contains(string(raw), "stalls without a report since the previous one: 2\n") {
		t.Fatalf("second report does not count the two it stands for:\n%s", raw)
	}
	if !strings.Contains(string(raw), "ended: ") || !strings.Contains(string(raw), "after 5.25s") {
		t.Fatalf("second report lacks its end and length:\n%s", raw)
	}
}

func TestOnlyTheNewestReportsAreKept(t *testing.T) {
	dir := t.TempDir()
	w, c, _ := newManual(t, Options{Threshold: time.Second, Dir: dir, MinInterval: time.Second, Keep: 3})
	var names []string
	for i := 0; i < 5; i++ {
		end := w.Begin("metrics store write")
		for _, ev := range run(w, c, 2*time.Second) {
			if ev.kind == eventStall {
				names = append(names, ev.name)
			}
		}
		end()
		run(w, c, 10*time.Second)
	}
	reports := Reports(dir)
	if len(names) != 5 || len(reports) != 3 {
		t.Fatalf("%d stalls reported, %d files kept; want 5 and 3", len(names), len(reports))
	}
	for i, path := range reports {
		if filepath.Base(path) != names[i+2] {
			t.Errorf("kept %s, want %s", filepath.Base(path), names[i+2])
		}
	}
	if at, ok := ReportTime(reports[0]); !ok || at.IsZero() {
		t.Errorf("report name %s does not give its time", reports[0])
	}
}

// A wait that does not end is captured a second time, and one whose end can
// never run is forgotten instead of holding the stall open for ever.
func TestAWaitThatNeverEndsIsCapturedTwiceThenForgotten(t *testing.T) {
	dir := t.TempDir()
	capture := 0
	w, c, logged := newManual(t, Options{Threshold: time.Second, Dir: dir, MinInterval: time.Second})
	w.opts.stacks = func() ([]byte, bool) {
		capture++
		return []byte("capture " + string(rune('0'+capture)) + "\n"), capture == 2
	}
	_ = w.Begin("database statement") // its end never runs
	run(w, c, 1500*time.Millisecond)
	run(w, c, 3*time.Second)
	if capture != 1 {
		t.Fatalf("%d captures before ten thresholds, want 1", capture)
	}
	run(w, c, 6*time.Second)
	run(w, c, time.Second)
	if capture != 2 {
		t.Fatalf("%d captures after ten thresholds, want 2", capture)
	}
	raw, _ := os.ReadFile(Reports(dir)[0])
	for _, want := range []string{"capture 1\n", "still waiting at ", "goroutines (cut at 8388608 bytes):\n\ncapture 2\n"} {
		if !strings.Contains(string(raw), want) {
			t.Errorf("report lacks %q:\n%s", want, raw)
		}
	}
	if !strings.Contains(logged.String(), "process stall continues") {
		t.Errorf("no line for the second capture: %s", logged.String())
	}
	events := run(w, c, 200*time.Second)
	if len(events) != 1 || events[0].kind != eventEnd {
		t.Fatalf("events once the wait is abandoned: %+v, want the end alone", events)
	}
	w.mu.Lock()
	left := len(w.waits)
	w.mu.Unlock()
	if left != 0 {
		t.Fatalf("%d waits still watched after being abandoned", left)
	}
	// The watch is free again for the next stall.
	end := w.Begin("log output")
	if events := run(w, c, 2*time.Second); len(events) != 1 || events[0].kind != eventStall {
		t.Fatalf("next stall not reported: %+v", events)
	}
	end()
}

func TestAProcessThatDidNotRunIsRecordedWithoutStacks(t *testing.T) {
	dir := t.TempDir()
	w, c, logged := newManual(t, Options{Threshold: 2 * time.Second, Dir: dir})
	step(w, c)
	c.add(500 * time.Millisecond)
	step(w, c)
	if len(Reports(dir)) != 0 {
		t.Fatal("a look on time was recorded as a pause")
	}
	c.add(7 * time.Second)
	step(w, c)
	reports := Reports(dir)
	if len(reports) != 1 {
		t.Fatalf("%d reports for a pause, want 1", len(reports))
	}
	raw, _ := os.ReadFile(reports[0])
	if !strings.Contains(string(raw), "the whole process did not run for 7s") || strings.Contains(string(raw), "goroutines") {
		t.Fatalf("pause report:\n%s", raw)
	}
	if !strings.Contains(logged.String(), "process pause recorded") || !strings.Contains(logged.String(), "duration=7s") {
		t.Fatalf("pause log line: %s", logged.String())
	}
}

func TestAReportThatCannotBeWrittenIsDroppedAndTheLineStillLogged(t *testing.T) {
	blocker := filepath.Join(t.TempDir(), "file")
	if err := os.WriteFile(blocker, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	w, c, logged := newManual(t, Options{Threshold: time.Second, Dir: filepath.Join(blocker, "stalls")})
	end := w.Begin("log output")
	run(w, c, 2*time.Second)
	end()
	run(w, c, time.Second)
	if !strings.Contains(logged.String(), "process stall recorded") {
		t.Fatalf("no log line when the report directory is unusable: %q", logged.String())
	}
}

func TestWithoutAWatchdogBeginDoesNothing(t *testing.T) {
	Use(nil)
	Begin("log output")()
	var w *Watchdog
	w.Begin("log output")()
	var out bytes.Buffer
	if n, err := Writer("log output", &out).Write([]byte("line\n")); n != 5 || err != nil || out.String() != "line\n" {
		t.Fatalf("watched writer wrote %d, %v, %q", n, err, out.String())
	}
}

func TestRealStacksNameTheCaller(t *testing.T) {
	stacks, cut := allStacks()
	if cut || !strings.Contains(string(stacks), "TestRealStacksNameTheCaller") {
		t.Fatalf("cut=%v, stacks:\n%s", cut, stacks)
	}
}
