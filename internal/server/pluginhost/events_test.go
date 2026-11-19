package pluginhost

import (
	"context"
	"slices"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/pkg/pluginsdk"
)

// A reader resumes the stderr ring at an offset; once the ring wrapped past
// it, it is told so and reads from the oldest byte kept; an offset past the
// end (logs forgotten since) reads the whole ring, as truncated.
func TestRingSinceReportsTruncation(t *testing.T) {
	r := newRing(8)
	if data, from, next, cut := r.Since(0); len(data) != 0 || from != 0 || next != 0 || cut {
		t.Fatalf("empty ring = %q %d %d %v", data, from, next, cut)
	}
	r.Write([]byte("abcde"))
	if data, from, next, cut := r.Since(2); string(data) != "cde" || from != 2 || next != 5 || cut {
		t.Fatalf("Since(2) = %q %d %d %v", data, from, next, cut)
	}
	r.Write([]byte("fghij")) // 10 written, "cdefghij" kept
	if data, from, next, cut := r.Since(5); string(data) != "fghij" || from != 5 || next != 10 || cut {
		t.Fatalf("Since(5) = %q %d %d %v", data, from, next, cut)
	}
	if data, from, next, cut := r.Since(1); string(data) != "cdefghij" || from != 2 || next != 10 || !cut {
		t.Fatalf("Since(1) after the wrap = %q %d %d %v", data, from, next, cut)
	}
	if data, _, next, cut := r.Since(10); len(data) != 0 || next != 10 || cut {
		t.Fatalf("Since(end) = %q %d %v", data, next, cut)
	}
	if data, from, next, cut := r.Since(99); string(data) != "cdefghij" || from != 2 || next != 10 || !cut {
		t.Fatalf("Since(past the end) = %q %d %d %v", data, from, next, cut)
	}
}

// The event ring keeps the last 256 events, numbered across the whole run;
// a reader behind the oldest is told it missed some.
func TestEventRingWrapsAndReportsTruncation(t *testing.T) {
	l := newPluginLogs()
	for i := range eventRingSize + 10 {
		l.record(Status{State: StateBackoff, Since: time.Unix(int64(i), 0), Params: map[string]any{"n": i}})
	}
	all, cut := l.eventsAfter(0)
	if len(all) != eventRingSize || !cut || all[0].Seq != 11 || all[len(all)-1].Seq != eventRingSize+10 {
		t.Fatalf("events after 0 = %d events from %d, truncated %v", len(all), all[0].Seq, cut)
	}
	tail, cut := l.eventsAfter(260)
	if len(tail) != 6 || cut || tail[0].Seq != 261 || tail[0].Params["n"] != 260 {
		t.Fatalf("events after 260 = %+v, truncated %v", tail, cut)
	}
	tail[0].Params["n"] = "changed"
	if again, _ := l.eventsAfter(260); again[0].Params["n"] != 260 {
		t.Fatal("a reader changed the kept event")
	}
	if none, cut := l.eventsAfter(eventRingSize + 10); len(none) != 0 || cut {
		t.Fatalf("events after the last = %+v %v", none, cut)
	}
	if reset, cut := l.eventsAfter(9999); len(reset) != eventRingSize || !cut {
		t.Fatalf("events after a forgotten cursor = %d %v", len(reset), cut)
	}
}

// A package's events and stderr outlive its process and a stop and start
// (a restart); a follower is woken by the next event; ForgetLogs drops
// them, and a later start begins new ones.
func TestLogsOutliveRestartsUntilForgotten(t *testing.T) {
	sp := &fakeSpawner{config: func(n int) pluginsdk.Config {
		c := sdkConfig("acme_backup")
		c.Start = func(_ context.Context, p *pluginsdk.Plugin) error {
			p.Logger().Info("hello from process", "n", n)
			return nil
		}
		return c
	}}
	h, _ := newTestHost(t, sp, nil, fastTiming())
	if _, ok := h.Logs("acme_backup", LogCursor{}); ok || h.Changed("acme_backup") != nil {
		t.Fatal("logs of a package that never started")
	}
	_ = h.Start(testSpec(t, "acme_backup"))
	waitStatus(t, h, "acme_backup", inState(StateRunning))
	waitLogs(t, h, "hello from process")
	first, ok := h.Logs("acme_backup", LogCursor{})
	if !ok || first.EventsTruncated || first.StderrTruncated || first.StderrFrom != 0 {
		t.Fatalf("first read = %+v", first)
	}
	if states := eventStates(first.Events); !slices.Equal(states[:2], []State{StateStarting, StateRunning}) {
		t.Fatalf("events = %v", states)
	}
	if first.Next.Event != first.Events[len(first.Events)-1].Seq || first.Next.Stderr != uint64(len(first.Stderr)) {
		t.Fatalf("next = %+v after %d events and %d bytes", first.Next, len(first.Events), len(first.Stderr))
	}

	follow := h.Changed("acme_backup")
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := h.Stop(ctx, "acme_backup"); err != nil {
		t.Fatal(err)
	}
	select {
	case <-follow:
	case <-time.After(5 * time.Second):
		t.Fatal("the stop woke no follower")
	}
	_ = h.Start(testSpec(t, "acme_backup"))
	waitStatus(t, h, "acme_backup", inState(StateRunning))
	waitLogs(t, h, `"n":2`)
	more, _ := h.Logs("acme_backup", first.Next)
	if states := eventStates(more.Events); !slices.Contains(states, StateStopped) || states[len(states)-1] != StateRunning ||
		more.Events[0].Seq != first.Next.Event+1 {
		t.Fatalf("events after the restart = %v from %d", states, more.Events[0].Seq)
	}
	if more.StderrFrom != first.Next.Stderr || strings.Contains(string(more.Stderr), `"n":1`) || !strings.Contains(string(more.Stderr), `"n":2`) {
		t.Fatalf("stderr after the restart = %q from %d", more.Stderr, more.StderrFrom)
	}

	if err := h.Stop(ctx, "acme_backup"); err != nil {
		t.Fatal(err)
	}
	h.ForgetLogs("acme_backup")
	if _, ok := h.Logs("acme_backup", LogCursor{}); ok || h.Stderr("acme_backup") != nil {
		t.Fatal("forgotten logs are still kept")
	}
	_ = h.Start(testSpec(t, "acme_backup"))
	waitStatus(t, h, "acme_backup", inState(StateRunning))
	fresh, _ := h.Logs("acme_backup", more.Next)
	if !fresh.EventsTruncated || fresh.Events[0].Seq != 1 || fresh.Events[0].State != StateStarting {
		t.Fatalf("a cursor of forgotten logs read %+v", fresh)
	}
}

func eventStates(events []Event) []State {
	out := make([]State, len(events))
	for i, e := range events {
		out[i] = e.State
	}
	return out
}

func waitLogs(t *testing.T, h *Host, want string) {
	t.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for !strings.Contains(string(h.Stderr("acme_backup")), want) {
		if time.Now().After(deadline) {
			t.Fatalf("stderr never had %q: %s", want, h.Stderr("acme_backup"))
		}
		time.Sleep(5 * time.Millisecond)
	}
}

