package engine

import (
	"fmt"
	"sort"
	"strings"
	"time"
)

// A start that hangs was ended only by its start timeout (two hours by
// default). While it waits for the health of a container it has just
// created, the agent watches three signals and declares the start stalled
// once every one of them is known and has been flat for startStallWindow:
//
//   - output: the container's last lines;
//   - cards: memory in use and utilization of the cards the instance is on;
//   - processes: CPU time, bytes read and written, major page faults, a
//     process in disk wait, and the set of the container's processes,
//     without the process that traces the others (a PRoot shim's own CPU
//     time says nothing about the engine it traces).
//
// A signal the agent cannot observe is unknown, never flat, and an unknown
// signal keeps a stall from being declared: an instance without cards, a
// card whose driver does not answer, a runtime that cannot name the
// container's processes (nerdctl, kube), a process whose counters cannot be
// read. The valid quiet phases each move at least one signal: reading
// weights from slow storage reads bytes or faults pages in, moving them to a
// card grows its memory, compiling or capturing a graph burns CPU or card
// time. Two waits can leave all three flat and are not watched: a rank of a
// multi-node unit waiting on a peer, and a start while another instance of
// this node is being started or stopped, whose lock or cache it may be
// waiting for.
//
// The verdict needs samples: at least startStallMinSamples of them with
// everything flat, none further than startStallMaxGap from the one before.
// A longer gap (a runtime CLI that did not answer, a suspended host) says
// nothing about what happened in it and starts the window over.
//
// The window is long against every start the repository records: the
// startup reference allows 240 to 420 seconds of initialization beyond the
// checkpoint read (docs/how-to/troubleshooting.md), and what has to stay
// flat for the whole of it is everything at once, not the output alone.
//
// Not detected: a start that advances, however slowly. The starts captured
// for GWKS-420 are of that kind (pkg/enginelog/testdata/field): their
// processes stood in tracing stop under a PRoot shim using a whole core,
// and still printed a line every few minutes. Nothing observed here tells
// that apart from a busy valid start under the same shim, so it is left to
// the start timeout, whose failure says what the processes were doing.
const (
	startStallWindow     = 20 * time.Minute
	startStallInterval   = time.Minute
	startStallMaxGap     = 3 * startStallInterval
	startStallMinSamples = 10
)

// Thresholds below which a change between two samples is noise, not progress.
const (
	stallCardMemoryBytes = 64 << 20 // card memory, against the last value that moved
	stallCardUtilization = 5.0      // percent, on any card
	stallCPUShare        = 0.2      // of one core, over the time between samples
	stallIOBytesPerSec   = 16 << 10
	stallFaultsPerSec    = 1.0
	stallClockTicks      = 100 // USER_HZ, what /proc reports CPU time in
)

// startStall configures the watch; the zero value uses the constants. Tests
// shorten it and point the process reads at a fixture.
type startStall struct {
	window, interval, maxGap time.Duration
	minSamples               int
	proc                     procFS
}

func (c startStall) windowOrDefault() time.Duration {
	if c.window > 0 {
		return c.window
	}
	return startStallWindow
}

func (c startStall) intervalOrDefault() time.Duration {
	if c.interval > 0 {
		return c.interval
	}
	return startStallInterval
}

func (c startStall) watch() *stallWatch {
	w := &stallWatch{window: c.windowOrDefault(), maxGap: c.maxGap, minSamples: c.minSamples, tracerShare: -1}
	if w.maxGap <= 0 {
		w.maxGap = startStallMaxGap
	}
	if w.minSamples <= 0 {
		w.minSamples = startStallMinSamples
	}
	return w
}

// stallSample is one observation of a starting instance, without its cards:
// those are asked for only when the rest is flat (stallWatch.observe).
type stallSample struct {
	outputKnown bool
	output      string // fingerprint of the last lines

	// procsRead says the container's processes could be listed; procsKnown
	// that every counter the watch compares could be read as well.
	procsRead, procsKnown bool
	procs                 processSample

	// peerBusy: another instance of this node is being started or stopped.
	peerBusy bool
}

// cardSample is memory in use, summed over the instance's cards, and the
// highest utilization of any of them; known is false when a card did not
// answer both.
type cardSample struct {
	memory, util float64
	known        bool
}

// stallWatch decides from successive samples (startStall.watch makes one).
type stallWatch struct {
	window, maxGap time.Duration
	minSamples     int
	// Since when each signal has been flat; zero while it is unknown.
	outputFlat, cardsFlat, procsFlat time.Time
	// flatSamples counts the samples in a row that found every signal known
	// and unmoved.
	flatSamples int
	prev        stallSample
	prevAt      time.Time
	memoryRef   float64
	// tracerShare is the tracers' share of a core over the last interval,
	// negative while unknown; reported, never judged.
	tracerShare float64
}

// observe takes the sample made at now and reports whether the start is
// stalled. cards is asked only when the output and the processes are known
// and flat at this sample: a start that shows progress elsewhere needs no
// answer from the card tool, which the node's metrics collection shares.
func (w *stallWatch) observe(now time.Time, s stallSample, cards func() cardSample) bool {
	elapsed := now.Sub(w.prevAt)
	if w.prevAt.IsZero() || elapsed > w.maxGap || s.peerBusy {
		// Nothing to compare with, or nothing known about what happened
		// since: the window starts here.
		w.outputFlat, w.cardsFlat, w.procsFlat, w.flatSamples, w.tracerShare = time.Time{}, time.Time{}, time.Time{}, 0, -1
		w.prev, w.prevAt = s, now
		if s.peerBusy {
			return false
		}
		if s.outputKnown {
			w.outputFlat = now
		}
		if s.procsKnown {
			w.procsFlat = now
		}
		return false
	}

	moved := false
	switch {
	case !s.outputKnown:
		w.outputFlat = time.Time{}
	case w.outputFlat.IsZero() || s.output != w.prev.output:
		w.outputFlat, moved = now, true
	}
	switch {
	case !s.procsKnown:
		w.procsFlat = time.Time{}
	case w.procsFlat.IsZero() || !w.prev.procsKnown || s.procs.movedSince(w.prev.procs, elapsed):
		w.procsFlat, moved = now, true
	}
	w.tracerShare = -1
	if s.procsRead && w.prev.procsRead {
		w.tracerShare = s.procs.tracerShareSince(w.prev.procs, elapsed)
	}

	if moved || w.outputFlat.IsZero() || w.procsFlat.IsZero() {
		w.cardsFlat = time.Time{} // not asked: unknown, and judged afresh when next asked
	} else {
		switch c := cards(); {
		case !c.known:
			w.cardsFlat = time.Time{}
		case w.cardsFlat.IsZero():
			w.cardsFlat, w.memoryRef, moved = now, c.memory, true
		case c.util >= stallCardUtilization:
			w.cardsFlat, moved = now, true
		case c.memory-w.memoryRef >= stallCardMemoryBytes || w.memoryRef-c.memory >= stallCardMemoryBytes:
			w.cardsFlat, w.memoryRef, moved = now, c.memory, true
		}
	}

	w.prev, w.prevAt = s, now
	held := func(since time.Time) bool { return !since.IsZero() && now.Sub(since) >= w.window }
	if moved || w.outputFlat.IsZero() || w.procsFlat.IsZero() || w.cardsFlat.IsZero() {
		w.flatSamples = 0
		return false
	}
	w.flatSamples++
	return w.flatSamples >= w.minSamples && held(w.outputFlat) && held(w.cardsFlat) && held(w.procsFlat)
}

// detail says, for the failure, how long each signal has been flat and what
// state the container's processes are in.
func (w *stallWatch) detail(now time.Time) string {
	flat := func(since time.Time) string { return now.Sub(since).Round(time.Second).String() }
	out := fmt.Sprintf("no progress: output flat %s, cards flat %s, processes flat %s", flat(w.outputFlat), flat(w.cardsFlat), flat(w.procsFlat))
	if states := w.processes(); states != "" {
		out += "; " + states
	}
	return out
}

// processes describes the container's processes as last sampled, with the
// tracer's share of a core over the last interval when there is one; ""
// when they could not be listed.
func (w *stallWatch) processes() string {
	if !w.prev.procsRead {
		return ""
	}
	out := w.prev.procs.summary()
	if out != "" && w.tracerShare >= 0 {
		out += fmt.Sprintf(" at %.0f%% of a core", w.tracerShare*100)
	}
	return out
}

// processSample is what /proc says of a container's processes at one moment.
type processSample struct {
	pids     string // the processes counted, as a sorted list
	cpuTicks uint64 // user+system time of all but the tracers
	ioBytes  uint64 // bytes read and written, by all but the tracers
	faults   uint64 // major page faults, by all but the tracers
	blocked  bool   // one of them is in uninterruptible (disk) wait
	states   []processState
	// The tracers' own CPU time and how many processes they trace.
	tracerTicks uint64
	traced      int
}

// processState is one process of the snapshot the failure carries.
type processState struct {
	pid    int
	name   string
	state  byte // the kernel's state letter: R, S, D, t (tracing stop), Z, ...
	tracer bool // it traces other processes of the container
}

// movedSince reports progress against the sample taken elapsed ago.
func (p processSample) movedSince(prev processSample, elapsed time.Duration) bool {
	seconds := elapsed.Seconds()
	if seconds <= 0 {
		return true
	}
	switch {
	case p.pids != prev.pids, p.blocked:
		return true
	case p.cpuTicks < prev.cpuTicks || float64(p.cpuTicks-prev.cpuTicks) >= stallCPUShare*seconds*stallClockTicks:
		return true
	case p.faults < prev.faults || float64(p.faults-prev.faults) >= stallFaultsPerSec*seconds:
		return true
	case p.ioBytes < prev.ioBytes || float64(p.ioBytes-prev.ioBytes) >= stallIOBytesPerSec*seconds:
		return true
	}
	return false
}

// tracerShareSince is the tracers' share of one core since the sample taken
// elapsed ago, negative when it cannot be told.
func (p processSample) tracerShareSince(prev processSample, elapsed time.Duration) float64 {
	if p.traced == 0 || prev.traced == 0 || p.tracerTicks < prev.tracerTicks || elapsed <= 0 {
		return -1
	}
	return float64(p.tracerTicks-prev.tracerTicks) / (elapsed.Seconds() * stallClockTicks)
}

var processStateNames = map[byte]string{
	'R': "running", 'S': "sleeping", 'D': "in disk wait", 't': "in tracing stop", 'T': "stopped", 'Z': "zombie", 'I': "idle",
}

// summary groups the processes by state, most telling first, and names the
// ones that are not simply asleep: "2 in tracing stop (worker_a, worker_b),
// 5 sleeping; tracer proot running".
func (p processSample) summary() string {
	if len(p.states) == 0 {
		return ""
	}
	byState := map[byte][]string{}
	var tracers []string
	for _, s := range p.states {
		if s.tracer {
			tracers = append(tracers, fmt.Sprintf("tracer %s %s", s.name, stateName(s.state)))
			continue
		}
		byState[s.state] = append(byState[s.state], s.name)
	}
	order := make([]byte, 0, len(byState))
	for state := range byState {
		order = append(order, state)
	}
	rank := func(state byte) int { return strings.IndexByte("tTDZRIS", state) }
	sort.Slice(order, func(i, j int) bool {
		ri, rj := rank(order[i]), rank(order[j])
		if ri < 0 || rj < 0 || ri == rj {
			return order[i] < order[j]
		}
		return ri < rj
	})
	parts := make([]string, 0, len(order))
	for _, state := range order {
		names := byState[state]
		part := fmt.Sprintf("%d %s", len(names), stateName(state))
		if state != 'S' && state != 'I' {
			shown := names
			if len(shown) > 3 {
				shown = shown[:3]
			}
			part += " (" + strings.Join(shown, ", ") + ")"
		}
		parts = append(parts, part)
	}
	out := "processes: " + strings.Join(parts, ", ")
	if len(tracers) > 0 {
		out += "; " + strings.Join(tracers, ", ")
	}
	return out
}

func stateName(state byte) string {
	if name, ok := processStateNames[state]; ok {
		return name
	}
	return "in state " + string(state)
}
