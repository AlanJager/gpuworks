package store

import (
	"sort"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// ResidencyState is what an instance was doing with its cards during a
// stretch of the residency timeline.
type ResidencyState string

const (
	// ResidencyAwake: running, serving.
	ResidencyAwake ResidencyState = "awake"
	// ResidencyAsleep: its engine slept; it keeps only the residue (Level 1
	// or 2 says how deep).
	ResidencyAsleep ResidencyState = "asleep"
	// ResidencySwitching: between a sleep or wake order and its result.
	ResidencySwitching ResidencyState = "switching"
	// ResidencyStarting: pulled, created or loading, not yet serving.
	ResidencyStarting ResidencyState = "starting"
)

// ResidencyInstance is one instance that held cards in the window: a live row,
// or one removed during it (known from its INSTANCE_REMOVED event).
type ResidencyInstance struct {
	UUID         string
	DeploymentID uint
	NodeID       uint
	GPUs         []int
	// Live says the row still exists; Start is then its creation time.
	Live  bool
	Start time.Time
	// EventsFrom is the instance whose state and sleep events say what this
	// one did: a headless follower sleeps and wakes with its leader's engine
	// and reports none of its own. Empty means its own events.
	EventsFrom string
	// State, Phase and SleepLevel are the live row's own values, used only
	// when no event says anything about the instance.
	State, Phase string
	SleepLevel   int
}

// ResidencyEvent is one state or sleep event, decoded.
type ResidencyEvent struct {
	ID     uint
	Code   string
	At     time.Time
	State  string
	Phase  string
	Level  int
	Result string
	NodeID uint
	GPUs   []int
}

// ResidencySegment is one stretch of an instance holding a card in one state.
type ResidencySegment struct {
	DeploymentID uint
	Instance     string
	State        ResidencyState
	// Level is the sleep level while asleep.
	Level    int
	From, To time.Time
}

// residencyMachine is the instance's state as the events leave it.
type residencyMachine struct {
	alive        bool
	state, phase string
	level        int
}

func (m *residencyMachine) apply(ev ResidencyEvent) {
	switch ev.Code {
	case instanceEventCreated:
		*m = residencyMachine{alive: true, state: ev.State}
	case instanceEventStatusUpdated, instanceEventStateChanged:
		m.alive, m.state, m.phase = true, ev.State, ev.Phase
	case instanceEventSleepResult:
		// A rolled-back order leaves the level where it was.
		if ev.Result == "applied" {
			m.level = ev.Level
		}
	case EventInstanceRemoved:
		m.alive = false
	}
}

// label is what the instance is doing, ok false while it holds no card.
func (m *residencyMachine) label() (ResidencyState, int, bool) {
	if !m.alive {
		return "", 0, false
	}
	switch m.state {
	case proto.InstPending, proto.InstPulling, proto.InstStarting, proto.InstOptimizing:
		return ResidencyStarting, 0, true
	case proto.InstRunning, proto.InstDraining:
		if m.phase == proto.PhaseSleep {
			return ResidencySwitching, 0, true
		}
		if m.level > 0 {
			return ResidencyAsleep, m.level, true
		}
		return ResidencyAwake, 0, true
	case proto.InstFailed:
		// A failed sleep keeps its full claim until the agent confirms the
		// engine is gone.
		if m.phase == proto.PhaseSleep {
			return ResidencySwitching, 0, true
		}
	}
	return "", 0, false
}

// DeriveResidency is the instance's segments inside [from, to], clipped to the
// window. events are the instance's state and sleep events in id order: the
// latest ones before the window (their times are read as the window's start,
// they only say where it began) followed by those inside it. Adjacent
// stretches in the same state are one segment.
func DeriveResidency(inst ResidencyInstance, events []ResidencyEvent, from, to time.Time) []ResidencySegment {
	if !to.After(from) {
		return nil
	}
	var m residencyMachine
	if len(events) == 0 {
		// Nothing was ever recorded (its events aged out): the row is all
		// there is, and it says what the instance is now.
		if !inst.Live {
			return nil
		}
		m = residencyMachine{alive: true, state: inst.State, phase: inst.Phase, level: inst.SleepLevel}
	}
	var out []ResidencySegment
	start := from
	if inst.Live && inst.Start.After(from) {
		// A live instance holds nothing before its row exists.
		start = inst.Start
	}
	emit := func(until time.Time) {
		if !until.After(start) {
			return
		}
		state, level, ok := m.label()
		if !ok {
			return
		}
		if n := len(out); n > 0 && out[n-1].State == state && out[n-1].Level == level && out[n-1].To.Equal(start) {
			out[n-1].To = until
			return
		}
		out = append(out, ResidencySegment{DeploymentID: inst.DeploymentID, Instance: inst.UUID, State: state, Level: level, From: start, To: until})
	}
	for _, ev := range events {
		at := ev.At
		if at.Before(from) {
			at = from
		}
		if at.After(to) {
			break
		}
		emit(at)
		if at.After(start) {
			start = at
		}
		m.apply(ev)
	}
	emit(to)
	return out
}

// SortResidencyEvents puts events in id order, the order they happened in.
func SortResidencyEvents(events []ResidencyEvent) {
	sort.Slice(events, func(i, j int) bool { return events[i].ID < events[j].ID })
}

// ResidencyMachine is an instance's state as its events leave it, for a
// reader that follows the events as they come (the scheduling activity)
// rather than deriving a window's segments: the same rules as
// DeriveResidency.
type ResidencyMachine struct{ m residencyMachine }

// Apply moves the machine by one event.
func (r *ResidencyMachine) Apply(ev ResidencyEvent) { r.m.apply(ev) }

// Label is what the instance is doing, ok false while it holds no card.
func (r *ResidencyMachine) Label() (ResidencyState, bool) {
	state, _, ok := r.m.label()
	return state, ok
}
