// Package stallwatch records what a process was doing when something it
// depends on stopped making progress.
//
// Code that may wait on storage (a log write, a database statement, a store
// flush) brackets the wait with Begin. A Watchdog that performs no I/O and
// takes no lock those waits hold notices one that outlasts the threshold,
// captures every goroutine's stack in memory at that moment, and hands the
// capture to a second goroutine that writes a report file and one log line.
// The capture therefore exists even when the disk or the logger is what
// stalled; the file appears as soon as writing works again.
//
// The package imports nothing from this repository: the server and the agent
// share it, and the logger itself reports through it.
package stallwatch

import (
	"context"
	"io"
	"log/slog"
	"runtime"
	"sort"
	"sync"
	"sync/atomic"
	"time"
)

const (
	// DefaultThreshold is how long a watched wait may last before it is
	// recorded. Storage that is merely slow answers within a second or two;
	// three seconds is past what a request can absorb unnoticed.
	DefaultThreshold = 3 * time.Second
	// DefaultKeep bounds the report files kept.
	DefaultKeep = 20
	// DefaultMinInterval is the least time between two reports: a disk that
	// stalls every few seconds must not fill the directory or the log.
	DefaultMinInterval = time.Minute
	// maxStackBytes bounds one capture. A capture that reaches it is cut and
	// says so.
	maxStackBytes = 8 << 20
	// stillAfter is the multiple of the threshold after which a wait that has
	// not ended is captured a second time, counted from the first capture
	// (taken one threshold in): two captures tell a slow operation (stacks
	// moved) from a hang (stacks identical).
	stillAfter = 10
	// abandonedAfter is the multiple of the threshold after which a wait is
	// dropped from the watch: a wait nothing will ever end (its goroutine is
	// gone for good) must not hold a stall open for ever. Every bracket in
	// this repository ends in a defer, so a panic does not leave one behind.
	abandonedAfter = 40
	// pauseFloor is the shortest time the watch may go unscheduled before
	// that is called a pause of the process: under CPU load a goroutine is
	// late by tens or hundreds of milliseconds without anything being wrong.
	pauseFloor = time.Second
	// queued bounds the events waiting for the reporting goroutine; each may
	// hold a capture of up to maxStackBytes.
	queued = 4
	// DefaultStopWait is how long Run waits for the reporting goroutine when
	// it stops. That goroutine may be writing to the very disk that stalled.
	DefaultStopWait = 2 * time.Second
)

// Options configure a Watchdog.
type Options struct {
	// Threshold is how long a watched wait may last; DefaultThreshold when
	// not positive.
	Threshold time.Duration
	// Dir receives the report files; empty keeps the log line only.
	Dir string
	// Keep bounds the report files; DefaultKeep when not positive.
	Keep int
	// MinInterval is the least time between reports; DefaultMinInterval when
	// not positive.
	MinInterval time.Duration
	// Log receives one line per recorded stall. It is called only from the
	// reporting goroutine, never from the watch itself.
	Log *slog.Logger
	// Header lines open every report (the program and its version).
	Header []string
	// StopWait is how long Run waits, once its context ends, for the
	// reports already queued to be written; DefaultStopWait when not
	// positive. Past it they are abandoned: stopping must not hang on a disk
	// that does not answer.
	StopWait time.Duration

	now    func() time.Time
	stacks func() ([]byte, bool)
}

// Watchdog watches the waits its Begin brackets.
type Watchdog struct {
	opts Options

	mu     sync.Mutex
	nextID uint64
	waits  map[uint64]wait

	// State of the watch goroutine alone.
	lastCheck  time.Time
	lastReport time.Time
	lastPause  time.Time
	episode    *episode
	suppressed int

	events chan event
	// written is the report the reporting goroutine last put on disk; that
	// goroutine alone reads and sets it.
	written string
	// dropped counts events the reporting goroutine was too far behind to
	// take.
	dropped atomic.Int64
}

type wait struct {
	site  string
	since time.Time
}

// Waiting is one watched wait as a report names it.
type Waiting struct {
	Site string
	For  time.Duration
}

type episode struct {
	name       string
	start      time.Time
	sites      map[string]bool
	suppressed bool
	followed   bool
	// reported is when the stall's stacks were taken.
	reported time.Time
}

type eventKind int

const (
	eventStall eventKind = iota
	eventStill
	eventEnd
	eventPause
)

type event struct {
	kind       eventKind
	name       string
	at         time.Time
	duration   time.Duration
	waiting    []Waiting
	sites      []string
	stacks     []byte
	cut        bool
	suppressed int
}

// New returns a Watchdog; Run starts it.
func New(opts Options) *Watchdog {
	if opts.Threshold <= 0 {
		opts.Threshold = DefaultThreshold
	}
	if opts.Keep <= 0 {
		opts.Keep = DefaultKeep
	}
	if opts.MinInterval <= 0 {
		opts.MinInterval = DefaultMinInterval
	}
	if opts.StopWait <= 0 {
		opts.StopWait = DefaultStopWait
	}
	if opts.Log == nil {
		opts.Log = slog.New(slog.DiscardHandler)
	}
	if opts.now == nil {
		opts.now = time.Now
	}
	if opts.stacks == nil {
		opts.stacks = allStacks
	}
	return &Watchdog{opts: opts, waits: map[uint64]wait{}, events: make(chan event, queued)}
}

var noop = func() {}

// Begin marks the start of a wait at site and returns what ends it. A nil
// Watchdog watches nothing.
func (w *Watchdog) Begin(site string) func() {
	if w == nil {
		return noop
	}
	since := w.opts.now()
	w.mu.Lock()
	w.nextID++
	id := w.nextID
	w.waits[id] = wait{site: site, since: since}
	w.mu.Unlock()
	return func() {
		w.mu.Lock()
		delete(w.waits, id)
		w.mu.Unlock()
	}
}

// Run watches until ctx ends, then gives the reporting goroutine StopWait to
// finish the events already queued. It returns after that whether or not the
// goroutine is done: one blocked on a disk that does not answer is left
// behind rather than waited for.
func (w *Watchdog) Run(ctx context.Context) {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for ev := range w.events {
			w.record(ev)
		}
	}()
	interval := w.interval()
	ticker := time.NewTicker(interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			close(w.events)
			wait := time.NewTimer(w.opts.StopWait)
			defer wait.Stop()
			select {
			case <-done:
			case <-wait.C:
			}
			return
		case <-ticker.C:
			for _, ev := range w.check(w.opts.now()) {
				select {
				case w.events <- ev:
				default:
					w.dropped.Add(1)
				}
			}
		}
	}
}

// interval is how often the watch looks: often enough that a stall's length
// is known to a quarter of the threshold.
func (w *Watchdog) interval() time.Duration {
	interval := w.opts.Threshold / 4
	if interval < 50*time.Millisecond {
		interval = 50 * time.Millisecond
	}
	if interval > time.Second {
		interval = time.Second
	}
	return interval
}

// check is one look at the watched waits. It does no I/O and logs nothing:
// what it finds leaves as events for the reporting goroutine.
func (w *Watchdog) check(now time.Time) []event {
	var events []event
	previous := w.lastCheck
	w.lastCheck = now
	// The watch itself was not scheduled: the whole process was paused
	// (frozen, starved of CPU, or stopped by the kernel while it reclaimed
	// memory). The stacks are from after the pause, so they are not kept.
	// A pause has its own allowance: recording one never costs the stall
	// that follows its report.
	if !previous.IsZero() {
		floor := w.opts.Threshold
		if floor < pauseFloor {
			floor = pauseFloor
		}
		gap := now.Sub(previous)
		if gap >= floor+w.interval() && (w.lastPause.IsZero() || now.Sub(w.lastPause) >= w.opts.MinInterval) {
			w.lastPause = now
			events = append(events, event{kind: eventPause, name: pauseName(now), at: now, duration: gap})
		}
	}

	over := w.over(now)
	switch {
	case w.episode == nil && len(over) > 0:
		w.episode = &episode{start: now.Add(-over[0].For), sites: map[string]bool{}, suppressed: true}
		w.suppressed++
	case w.episode != nil && len(over) == 0:
		if !w.episode.suppressed {
			events = append(events, event{kind: eventEnd, name: w.episode.name, at: now,
				duration: now.Sub(w.episode.start), sites: sortedSites(w.episode.sites)})
		}
		w.episode = nil
	}
	// A stall that began too soon after the previous report is not skipped
	// for good: it is looked at again on every tick, so one that lasts gets
	// its report as soon as the interval allows.
	if w.episode != nil && w.episode.suppressed && len(over) > 0 && w.allow(now) {
		w.episode.suppressed = false
		w.episode.reported = now
		w.suppressed--
		stacks, cut := w.opts.stacks()
		w.episode.name = reportName(now)
		events = append(events, event{kind: eventStall, name: w.episode.name, at: now, duration: over[0].For,
			waiting: over, stacks: stacks, cut: cut, suppressed: w.takeSuppressed()})
	}
	if w.episode != nil {
		for _, waiting := range over {
			w.episode.sites[waiting.Site] = true
		}
		if !w.episode.suppressed && !w.episode.followed && len(over) > 0 && now.Sub(w.episode.reported) >= (stillAfter-1)*w.opts.Threshold {
			w.episode.followed = true
			// A second capture is worth its memory only when the reporting
			// goroutine is keeping up; behind, the first one is what counts.
			var stacks []byte
			cut := false
			if len(w.events) == 0 {
				stacks, cut = w.opts.stacks()
			}
			events = append(events, event{kind: eventStill, name: w.episode.name, at: now, duration: over[0].For,
				waiting: over, stacks: stacks, cut: cut})
		}
	}
	return events
}

// allow says whether a report may be made now, and counts it when so.
func (w *Watchdog) allow(now time.Time) bool {
	if !w.lastReport.IsZero() && now.Sub(w.lastReport) < w.opts.MinInterval {
		return false
	}
	w.lastReport = now
	return true
}

func (w *Watchdog) takeSuppressed() int {
	n := w.suppressed
	w.suppressed = 0
	return n
}

// over lists the waits past the threshold, longest first, and forgets the
// ones nothing will ever end.
func (w *Watchdog) over(now time.Time) []Waiting {
	var over []Waiting
	w.mu.Lock()
	for id, waiting := range w.waits {
		age := now.Sub(waiting.since)
		if age >= abandonedAfter*w.opts.Threshold {
			delete(w.waits, id)
			continue
		}
		if age >= w.opts.Threshold {
			over = append(over, Waiting{Site: waiting.site, For: age})
		}
	}
	w.mu.Unlock()
	sort.Slice(over, func(i, j int) bool {
		if over[i].For != over[j].For {
			return over[i].For > over[j].For
		}
		return over[i].Site < over[j].Site
	})
	return over
}

func sortedSites(sites map[string]bool) []string {
	out := make([]string, 0, len(sites))
	for site := range sites {
		out = append(out, site)
	}
	sort.Strings(out)
	return out
}

// allStacks is every goroutine's stack as a crash prints them, cut at
// maxStackBytes.
func allStacks() ([]byte, bool) {
	size := 256 << 10
	for {
		buf := make([]byte, size)
		n := runtime.Stack(buf, true)
		if n < size {
			return buf[:n], false
		}
		if size >= maxStackBytes {
			return buf, true
		}
		size *= 2
	}
}

var current atomic.Pointer[Watchdog]

// Use makes w the process's watchdog: the one the package-level Begin and
// Writer report to. Nil stops the watching.
func Use(w *Watchdog) { current.Store(w) }

// Begin marks the start of a wait at site for the process's watchdog and
// returns what ends it; it costs one atomic load while there is none.
func Begin(site string) func() { return current.Load().Begin(site) }

// Writer returns out with every Write watched as site by the process's
// watchdog, whichever is in use when the write happens.
func Writer(site string, out io.Writer) io.Writer { return watchedWriter{site: site, out: out} }

type watchedWriter struct {
	site string
	out  io.Writer
}

func (w watchedWriter) Write(p []byte) (int, error) {
	defer Begin(w.site)()
	return w.out.Write(p)
}
