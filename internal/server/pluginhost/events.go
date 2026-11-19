package pluginhost

import (
	"maps"
	"sync"
	"time"
)

// eventRingSize is how many supervision events a package keeps.
const eventRingSize = 256

// Event is one change of a plugin's supervision state, as an administrator
// reads it back (GET /api/v1/plugins/{id}/logs).
type Event struct {
	// Seq numbers the package's events from 1, across its processes and
	// its restarts.
	Seq   uint64
	At    time.Time
	State State
	// Code and Params say why it entered State; empty when nothing needs
	// saying.
	Code   string
	Params map[string]any
}

// pluginLogs are one package's supervision events and stderr output. Like
// its metrics they outlive its processes, its restarts and its disable and
// enable; Forget drops them when it is uninstalled.
type pluginLogs struct {
	stderr *ring

	mu      sync.Mutex
	events  [eventRingSize]Event
	seq     uint64        // the last event's Seq; 0 before the first
	changed chan struct{} // closed and replaced on every event and stderr write
}

func newPluginLogs() *pluginLogs {
	l := &pluginLogs{stderr: newRing(stderrRingBytes), changed: make(chan struct{})}
	l.stderr.onWrite = l.signal
	return l
}

// signal wakes every reader waiting for more.
func (l *pluginLogs) signal() {
	l.mu.Lock()
	close(l.changed)
	l.changed = make(chan struct{})
	l.mu.Unlock()
}

// record keeps st's state as the next event, dropping the oldest once 256
// are kept.
func (l *pluginLogs) record(st Status) {
	l.mu.Lock()
	l.seq++
	l.events[(l.seq-1)%eventRingSize] = Event{Seq: l.seq, At: st.Since.UTC(), State: st.State, Code: st.Code, Params: maps.Clone(st.Params)}
	close(l.changed)
	l.changed = make(chan struct{})
	l.mu.Unlock()
}

// eventsAfter returns the kept events after seq after, oldest first, and
// whether some after it were already dropped. A seq past the last is one
// of logs since forgotten: it reads every kept event, as truncated.
func (l *pluginLogs) eventsAfter(after uint64) ([]Event, bool) {
	l.mu.Lock()
	defer l.mu.Unlock()
	reset := after > l.seq
	if reset {
		after = 0
	}
	oldest := uint64(1)
	if l.seq > eventRingSize {
		oldest = l.seq - eventRingSize + 1
	}
	truncated := reset || after+1 < oldest
	from := max(after+1, oldest)
	var out []Event
	for s := from; s <= l.seq; s++ {
		e := l.events[(s-1)%eventRingSize]
		e.Params = maps.Clone(e.Params)
		out = append(out, e)
	}
	return out, truncated
}

// LogCursor is where a reader of a package's logs stands: the last event
// it read and the stderr offset it reads from next.
type LogCursor struct {
	Event  uint64
	Stderr uint64
}

// Logs are what a package logged after a cursor.
type Logs struct {
	// Events are the supervision events after the cursor, oldest first;
	// EventsTruncated reports that older ones after it were dropped.
	Events          []Event
	EventsTruncated bool
	// Stderr is the plugin's stderr output from StderrFrom on;
	// StderrTruncated reports that the ring had dropped some of what the
	// cursor asked for, so StderrFrom is past it.
	Stderr          []byte
	StderrFrom      uint64
	StderrTruncated bool
	// Next is the cursor to read on from.
	Next LogCursor
}

// Logs returns what package id logged after from: its supervision events
// (the last 256) and its stderr output (the last 1 MiB), across its
// processes and restarts until it is uninstalled. ok is false when the host
// keeps no logs for id: it never started here, or was forgotten.
func (h *Host) Logs(id string, from LogCursor) (out Logs, ok bool) {
	h.mu.Lock()
	l := h.logs[id]
	h.mu.Unlock()
	if l == nil {
		return Logs{Next: from}, false
	}
	out.Events, out.EventsTruncated = l.eventsAfter(from.Event)
	out.Next.Event = from.Event
	if n := len(out.Events); n > 0 {
		out.Next.Event = out.Events[n-1].Seq
	}
	out.Stderr, out.StderrFrom, out.Next.Stderr, out.StderrTruncated = l.stderr.Since(from.Stderr)
	return out, true
}

// Changed returns a channel closed at package id's next supervision event
// or stderr write, for a reader that follows its logs; nil (never closed)
// when the host keeps no logs for id.
func (h *Host) Changed(id string) <-chan struct{} {
	h.mu.Lock()
	l := h.logs[id]
	h.mu.Unlock()
	if l == nil {
		return nil
	}
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.changed
}

// Forget drops everything the host keeps of package id once it is no
// longer supervised: its stopped instance (whose status Status reported),
// its counters and its logs. The bridge calls it when the package is
// uninstalled, so nothing of it stays in memory. A plugin still running
// keeps its instance and the counters and logs it started with until it
// stops.
func (h *Host) Forget(id string) {
	h.mu.Lock()
	if i := h.plugins[id]; i != nil && i.finished() {
		delete(h.plugins, id)
	}
	delete(h.metrics, id)
	delete(h.logs, id)
	h.mu.Unlock()
}

func (h *Host) logsLocked(id string) *pluginLogs {
	l := h.logs[id]
	if l == nil {
		l = newPluginLogs()
		h.logs[id] = l
	}
	return l
}
