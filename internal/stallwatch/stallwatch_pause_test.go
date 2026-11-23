package stallwatch

import (
	"strings"
	"testing"
	"time"
)

// A wait that began before a pause and is still open after it is a stall
// once it has waited the threshold while the process ran.
func TestAWaitOpenAcrossAPauseIsReportedForTheTimeTheProcessRan(t *testing.T) {
	dir := t.TempDir()
	w, c, _ := newManual(t, Options{Threshold: time.Second, Dir: dir})
	step(w, c)
	_ = w.Begin("log output")
	run(w, c, 500*time.Millisecond)
	c.add(5 * time.Second) // frozen
	if events := step(w, c); len(events) != 1 || events[0].kind != eventPause {
		t.Fatalf("events at the look after the pause: %+v, want the pause alone", events)
	}
	// It had waited 500ms before the pause and the look after the pause is
	// one ordinary interval (250ms) of running time later: 750ms.
	events := run(w, c, 250*time.Millisecond)
	if len(events) != 1 || events[0].kind != eventStall {
		t.Fatalf("events once the wait had run a threshold: %+v, want its report", events)
	}
	if waited := events[0].waiting[0].For; waited != time.Second {
		t.Fatalf("the report says the wait lasted %s; the pause is not part of it", waited)
	}
}

// A wait the watch gives up on is said to have never returned also when its
// stall never got a report (it stayed inside the minimum interval of the
// previous one) and when no stall was open at all: silence would read as
// "nothing happened".
func TestAWaitGivenUpOnWithoutAReportIsStillRecorded(t *testing.T) {
	dir := t.TempDir()
	// 40 thresholds are 40s, inside the minute after the first report.
	w, c, logged := newManual(t, Options{Threshold: time.Second, Dir: dir})
	end := w.Begin("log output")
	run(w, c, 1500*time.Millisecond)
	end()
	run(w, c, time.Second)
	if len(Reports(dir)) != 1 {
		t.Fatalf("%d reports for the first stall, want 1", len(Reports(dir)))
	}
	_ = w.Begin("database commit") // never returns, and gets no report
	events := run(w, c, 41*time.Second)
	if len(events) != 1 || events[0].kind != eventUnended || events[0].name != "" || events[0].waiting[0].Site != "database commit" {
		t.Fatalf("events for a wait given up on inside the minimum interval: %+v, want the one that says it never returned, with no file", events)
	}
	if len(Reports(dir)) != 1 {
		t.Fatalf("%d reports, want the first stall's alone", len(Reports(dir)))
	}
	line := logged.String()[strings.LastIndex(logged.String(), "process stall no longer watched"):]
	if !strings.Contains(line, `site="database commit"`) || strings.Contains(line, "report=") {
		t.Fatalf("log line for a wait given up on without a report: %s", line)
	}

	// No stall open: the clock jumps from under the threshold to past 40
	// of them between two looks that are not far enough apart for a pause.
	w2, c2, logged2 := newManual(t, Options{Threshold: 50 * time.Millisecond, Dir: t.TempDir()})
	step(w2, c2)
	_ = w2.Begin("metrics store write")
	w2.mu.Lock()
	for id, waiting := range w2.waits {
		waiting.since = waiting.since.Add(-time.Hour)
		w2.waits[id] = waiting
	}
	w2.mu.Unlock()
	c2.add(w2.interval())
	events = step(w2, c2)
	if len(events) != 1 || events[0].kind != eventUnended || events[0].name != "" {
		t.Fatalf("events for a wait given up on with no stall open: %+v", events)
	}
	if !strings.Contains(logged2.String(), "process stall no longer watched: a wait never returned") || !strings.Contains(logged2.String(), `site="metrics store write"`) {
		t.Fatalf("log: %s", logged2.String())
	}
}
