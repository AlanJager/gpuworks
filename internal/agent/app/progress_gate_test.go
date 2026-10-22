package app

import (
	"strconv"
	"sync"
	"testing"
	"time"
)

// fakeGateClock replaces both the gate's clock and its timer so the trailing
// flush can be driven deterministically instead of slept for.
type fakeGateClock struct {
	mu      sync.Mutex
	now     time.Time
	pending []*fakeGateTimer
}

type fakeGateTimer struct {
	clock   *fakeGateClock
	fire    func()
	at      time.Time
	stopped bool
}

func (timer *fakeGateTimer) Stop() bool {
	timer.clock.mu.Lock()
	defer timer.clock.mu.Unlock()
	already := timer.stopped
	timer.stopped = true
	return !already
}

func newFakeGateClock(start time.Time) *fakeGateClock {
	return &fakeGateClock{now: start}
}

func (c *fakeGateClock) Now() time.Time {
	c.mu.Lock()
	defer c.mu.Unlock()
	return c.now
}

func (c *fakeGateClock) advance(d time.Duration) {
	c.mu.Lock()
	c.now = c.now.Add(d)
	c.mu.Unlock()
}

func (c *fakeGateClock) after(d time.Duration, fire func()) gateTimer {
	c.mu.Lock()
	defer c.mu.Unlock()
	timer := &fakeGateTimer{clock: c, fire: fire, at: c.now.Add(d)}
	c.pending = append(c.pending, timer)
	return timer
}

// fireDue runs every armed timer whose deadline has been reached and reports
// how many fired. Timers are one-shot, like time.AfterFunc.
func (c *fakeGateClock) fireDue() int {
	c.mu.Lock()
	now := c.now
	due := make([]*fakeGateTimer, 0, len(c.pending))
	keep := c.pending[:0]
	for _, timer := range c.pending {
		if timer.stopped {
			continue
		}
		if !timer.at.After(now) {
			timer.stopped = true
			due = append(due, timer)
			continue
		}
		keep = append(keep, timer)
	}
	c.pending = keep
	c.mu.Unlock()
	for _, timer := range due {
		timer.fire()
	}
	return len(due)
}

func (c *fakeGateClock) armed() int {
	c.mu.Lock()
	defer c.mu.Unlock()
	count := 0
	for _, timer := range c.pending {
		if !timer.stopped {
			count++
		}
	}
	return count
}

func newTestGate(clock *fakeGateClock) *progressGate {
	gate := newProgressGate()
	gate.now = clock.Now
	gate.after = clock.after
	return gate
}

func TestProgressGateKeepsOneReportPerSecondAndFlushesTheLastOne(t *testing.T) {
	clock := newFakeGateClock(time.Unix(100, 0))
	gate := newTestGate(clock)
	var sent []string
	report := func(message string) {
		gate.report("instance\x00a", "pulling\x00image_pull", false, func() { sent = append(sent, message) })
	}

	// 500 progress frames arriving at 50 Hz, i.e. ten seconds of a real pull.
	for i := 0; i < 500; i++ {
		report(strconv.Itoa(i))
		clock.advance(20 * time.Millisecond)
		clock.fireDue()
	}
	// Nothing new arrives; the trailing flush still has to deliver the last
	// suppressed frame rather than leaving the display one frame short.
	clock.advance(2 * time.Second)
	clock.fireDue()

	// Ten seconds at one report per second, plus the first frame, plus the
	// trailing flush. The exact figure is what matters: a limiter that lets a
	// second frame through per window would double it.
	if len(sent) > 12 {
		t.Fatalf("delivered %d reports for 10s of 50Hz progress, want at most 12", len(sent))
	}
	if len(sent) < 10 {
		t.Fatalf("delivered %d reports for 10s of progress, want at least one per second", len(sent))
	}
	if sent[0] != strconv.Itoa(0) {
		t.Fatalf("first delivered report = %q, want the first frame", sent[0])
	}
	if last := sent[len(sent)-1]; last != strconv.Itoa(499) {
		t.Fatalf("last delivered report = %q, want the newest frame %q", last, strconv.Itoa(499))
	}
}

func TestProgressGateDeliversMilestonesAndTerminalsWithoutDelay(t *testing.T) {
	clock := newFakeGateClock(time.Unix(100, 0))
	gate := newTestGate(clock)
	var sent []string
	send := func(key, milestone string, terminal bool, label string) {
		gate.report(key, milestone, terminal, func() { sent = append(sent, label) })
	}

	send("instance\x00a", "pulling\x00image_pull", false, "pull-first")
	clock.advance(10 * time.Millisecond)
	send("instance\x00a", "pulling\x00image_pull", false, "pull-progress")
	clock.advance(10 * time.Millisecond)
	// Same state, different phase: a milestone, so it must not wait.
	send("instance\x00a", "pulling\x00image_extract", false, "extract")
	clock.advance(10 * time.Millisecond)
	// Different state: also a milestone.
	send("instance\x00a", "starting\x00container_start", false, "starting")
	clock.advance(10 * time.Millisecond)
	send("instance\x00a", "starting\x00container_start", false, "starting-progress")
	clock.advance(10 * time.Millisecond)
	send("instance\x00a", "failed\x00", true, "failed")

	// Each milestone is preceded by the suppressed frame of the phase it ends:
	// that frame carries the phase's final counters, which the server folds
	// into the milestone's summary.
	want := []string{"pull-first", "pull-progress", "extract", "starting", "starting-progress", "failed"}
	if len(sent) != len(want) {
		t.Fatalf("delivered %v, want %v", sent, want)
	}
	for i := range want {
		if sent[i] != want[i] {
			t.Fatalf("delivered %v, want %v", sent, want)
		}
	}
	// Nothing is left to resurface after the terminal report.
	clock.advance(5 * time.Second)
	clock.fireDue()
	if len(sent) != len(want) {
		t.Fatalf("a frame was delivered after the terminal report: %v", sent)
	}
}

func TestProgressGateReleasesKeysOnTerminalStates(t *testing.T) {
	clock := newFakeGateClock(time.Unix(100, 0))
	gate := newTestGate(clock)
	for i := 0; i < 50; i++ {
		key := "instance\x00" + strconv.Itoa(i)
		gate.report(key, "pulling\x00image_pull", false, func() {})
		gate.report(key, "running\x00", false, func() {})
	}
	if got := gate.tracked(); got != 50 {
		t.Fatalf("tracked %d keys while 50 instances are live, want 50", got)
	}
	for i := 0; i < 50; i++ {
		gate.report("instance\x00"+strconv.Itoa(i), "stopped\x00", true, func() {})
	}
	if got := gate.tracked(); got != 0 {
		t.Fatalf("tracked %d keys after every instance reached a terminal state, want 0", got)
	}
}

func TestProgressGateDropsIdleKeysThatNeverReachATerminalState(t *testing.T) {
	clock := newFakeGateClock(time.Unix(100, 0))
	gate := newTestGate(clock)
	for i := 0; i < statusProgressPruneAfter+1; i++ {
		gate.report("modelfile\x00"+strconv.Itoa(i), "downloading", false, func() {})
	}
	if got := gate.tracked(); got != statusProgressPruneAfter+1 {
		t.Fatalf("tracked %d keys, want %d", got, statusProgressPruneAfter+1)
	}
	clock.advance(statusProgressIdleExpiry + time.Second)
	// One live report triggers the sweep; the abandoned transfers go away and
	// the reporting one stays.
	gate.report("modelfile\x00live", "downloading", false, func() {})
	if got := gate.tracked(); got != 1 {
		t.Fatalf("tracked %d keys after the idle sweep, want only the live one", got)
	}
}

func TestProgressGateKeepsObjectsIndependent(t *testing.T) {
	clock := newFakeGateClock(time.Unix(100, 0))
	gate := newTestGate(clock)
	var sent []string
	for i := 0; i < 20; i++ {
		key := "instance\x00" + strconv.Itoa(i)
		label := strconv.Itoa(i)
		gate.report(key, "pulling\x00image_pull", false, func() { sent = append(sent, label) })
	}
	if len(sent) != 20 {
		t.Fatalf("delivered %d first reports for 20 objects, want 20", len(sent))
	}
	// A second frame for one object is suppressed without affecting the others.
	clock.advance(10 * time.Millisecond)
	gate.report("instance\x000", "pulling\x00image_pull", false, func() { sent = append(sent, "extra") })
	if len(sent) != 20 {
		t.Fatalf("a same-object frame inside the interval was delivered: %v", sent[20:])
	}
	if clock.armed() != 1 {
		t.Fatalf("armed %d flush timers, want exactly one", clock.armed())
	}
}

// The last progress frame of a phase must survive the milestone that ends the
// phase: it carries the counters the milestone's summary is computed from.
func TestProgressGateFlushesAPhasesFinalFrameBeforeItsMilestone(t *testing.T) {
	clock := newFakeGateClock(time.Unix(100, 0))
	gate := newTestGate(clock)
	var sent []string
	pull := func(label string) {
		gate.report("instance\x00a", "pulling\x00image_pull", false, func() { sent = append(sent, label) })
	}
	pull("0 B")
	for i := 1; i <= 100; i++ {
		clock.advance(5 * time.Millisecond)
		pull(strconv.Itoa(i) + " MiB")
	}
	// The pull finishes and the next phase starts, all inside one second.
	clock.advance(5 * time.Millisecond)
	gate.report("instance\x00a", "pulling\x00image_verify", false, func() { sent = append(sent, "verify") })

	if len(sent) != 3 {
		t.Fatalf("delivered %v, want the first frame, the final frame and the milestone", sent)
	}
	if sent[1] != "100 MiB" {
		t.Fatalf("the frame delivered before the milestone = %q, want the phase's final one", sent[1])
	}
	if sent[2] != "verify" {
		t.Fatalf("the milestone must come last, got %v", sent)
	}
}

func TestProgressGateDiscardDropsTheSuppressedSample(t *testing.T) {
	clock := newFakeGateClock(time.Unix(100, 0))
	gate := newTestGate(clock)
	var sent []string
	report := func(label string) {
		gate.report("modelfile\x001-2", "downloading", false, func() { sent = append(sent, label) })
	}

	report("first")
	clock.advance(100 * time.Millisecond)
	report("suppressed")
	if clock.armed() != 1 {
		t.Fatalf("armed trailing flushes = %d, want 1", clock.armed())
	}
	gate.discard("modelfile\x001-2")
	if gate.tracked() != 0 {
		t.Fatalf("tracked keys after discard = %d, want 0", gate.tracked())
	}
	clock.advance(2 * time.Second)
	clock.fireDue()
	if len(sent) != 1 || sent[0] != "first" {
		t.Fatalf("delivered %v after discard, want only the first report", sent)
	}

	// The next producer for the same object starts afresh: its first report is
	// delivered at once instead of waiting out the discarded one's interval.
	report("next")
	if len(sent) != 2 || sent[1] != "next" {
		t.Fatalf("delivered %v, want the next producer's first report immediately", sent)
	}
	gate.discard("modelfile\x00unknown")
	var nilGate *progressGate
	nilGate.discard("modelfile\x001-2")
}

// A trailing flush that took the suppressed sample just before discard ran
// must not deliver it afterwards.
func TestProgressGateDiscardVoidsAFlushInFlight(t *testing.T) {
	clock := newFakeGateClock(time.Unix(100, 0))
	gate := newTestGate(clock)
	var sent []string
	gate.report("k", "downloading", false, func() { sent = append(sent, "first") })
	clock.advance(100 * time.Millisecond)
	gate.report("k", "downloading", false, func() { sent = append(sent, "suppressed") })

	gate.mu.Lock()
	entry := gate.keys["k"]
	send, seq := entry.pending, entry.seq
	entry.pending = nil
	gate.mu.Unlock()
	gate.discard("k")
	entry.deliver(seq, send)
	if len(sent) != 1 {
		t.Fatalf("delivered %v, want the in-flight flush voided by discard", sent)
	}
}
