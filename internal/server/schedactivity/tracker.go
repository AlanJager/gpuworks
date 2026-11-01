// Package schedactivity counts the scheduling activity as it happens
// (docs/reference/api.md, section 调度活动): the scheduling events of each
// reason, how often each card changed hands, and each deployment's yields,
// wakes, awake and held time. The store hands every event over once it is
// written (store.SetEventObserver); each adds O(1) to the minute it fell in.
// The last 24 hours are kept in memory, filled from the event log once when
// the server starts (store.ReplaySchedulingActivity); every finished hour is
// written whole to stats.db, which answers longer ranges. No read goes back
// to the event log.
package schedactivity

import (
	"sync"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

const (
	// Span is how far back the memory reaches.
	Span = 24 * time.Hour
	// minuteMS is one bucket.
	minuteMS = int64(time.Minute / time.Millisecond)
	// ringMinutes is the buckets kept: the span, the minute in progress and
	// one to spare while an hour is written.
	ringMinutes = int64(Span/time.Minute) + 62
	// pendingCap bounds the events held while the replay runs; past it the
	// oldest are dropped (the replay is read from the log anyway).
	pendingCap = 200_000
	// forgetMargin is how much older than the read of the live instances an
	// instance's last event must be for Forget to end it: an event's time is
	// read before its row is written.
	forgetMargin = time.Minute
)

var reasonCount = len(store.ActivityReasons())

// cardHit is one switch on a card; depHit one yield, yield-to or wake of a
// deployment.
type cardHit struct {
	node uint32
	gpu  int32
}

type hitKind uint8

const (
	hitYielded hitKind = iota
	hitYieldedTo
	hitWake
)

type depHit struct {
	dep  uint32
	kind hitKind
}

// nodeHit says a deployment hit (deps[hit] of the same minute) happened on
// a node: one per node of the cards the event concerns, the hits of one
// event next to each other, so that a read over several nodes counts the
// event once.
type nodeHit struct {
	hit  uint32
	node uint32
}

// bucket is one minute.
type bucket struct {
	minute   int64 // Unix minute it holds; a slot holding another is empty
	reasons  []int64
	cards    []cardHit
	deps     []depHit
	nodeHits []nodeHit
}

// spans is a set of stretches of time (Unix ms) kept merged: closed ones and,
// while count > 0, an open one from openAt.
type spans struct {
	closed [][2]int64
	count  int
	openAt int64
}

// change moves the count by delta at t, opening or closing the stretch.
func (s *spans) change(delta int, t int64) {
	before := s.count
	s.count += delta
	if s.count < 0 {
		s.count = 0
	}
	switch {
	case before == 0 && s.count > 0:
		s.openAt = t
	case before > 0 && s.count == 0:
		if t < s.openAt {
			t = s.openAt
		}
		if n := len(s.closed); n > 0 && s.closed[n-1][1] >= s.openAt {
			s.closed[n-1][1] = max(s.closed[n-1][1], t)
		} else if t > s.openAt {
			s.closed = append(s.closed, [2]int64{s.openAt, t})
		}
	}
}

// within is how long the stretches cover of [from, to) and the first and
// last instant they do (0 when none).
func (s *spans) within(from, to int64) (total, first, last int64) {
	add := func(a, b int64) {
		a, b = max(a, from), min(b, to)
		if b <= a {
			return
		}
		total += b - a
		if first == 0 || a < first {
			first = a
		}
		if b > last {
			last = b
		}
	}
	for _, c := range s.closed {
		add(c[0], c[1])
	}
	if s.count > 0 {
		add(s.openAt, to)
	}
	return total, first, last
}

// prune drops the stretches that ended before cutoff.
func (s *spans) prune(cutoff int64) {
	i := 0
	for i < len(s.closed) && s.closed[i][1] < cutoff {
		i++
	}
	if i > 0 {
		s.closed = append(s.closed[:0], s.closed[i:]...)
	}
}

// instance is what an instance is doing.
type instance struct {
	dep         uint
	m           store.ResidencyMachine
	awake, held bool
	// seen is when its last event happened (Unix ms).
	seen int64
}

// deployment is a deployment's awake and held time.
type deployment struct {
	awake, held spans
}

// Tracker is the memory of the last 24 hours.
type Tracker struct {
	mu        sync.Mutex
	ring      []bucket
	instances map[string]*instance
	deps      map[uint]*deployment
	nodes     map[string]uint32
	nodeNames []string

	// ready is false until the replay is in; meanwhile events wait in
	// pending. throughID is the last event the replay read: one the
	// observer hands over again is not counted twice.
	ready     bool
	throughID uint
	pending   []store.ObservedEvent
	dropped   int
	// startMS is when the memory begins: the replay's start.
	startMS int64
}

// NewTracker is an empty memory that holds what it observes until Ready.
func NewTracker() *Tracker {
	return &Tracker{
		ring: make([]bucket, ringMinutes), instances: map[string]*instance{}, deps: map[uint]*deployment{}, nodes: map[string]uint32{},
	}
}

// Observe takes one event (store.EventObserver).
func (t *Tracker) Observe(ev store.ObservedEvent) {
	t.mu.Lock()
	defer t.mu.Unlock()
	if !t.ready {
		if len(t.pending) >= pendingCap {
			t.pending = t.pending[1:]
			t.dropped++
		}
		t.pending = append(t.pending, ev)
		return
	}
	if ev.ID != 0 && ev.ID <= t.throughID {
		return
	}
	t.apply(ev)
}

// Replayed is a replay's events, handed in before Ready.
func (t *Tracker) Replayed(evs []store.ObservedEvent) {
	t.mu.Lock()
	defer t.mu.Unlock()
	for _, ev := range evs {
		t.apply(ev)
	}
}

// Ready ends the replay: it read the log up to throughID from startMS on.
// The events observed meanwhile that the replay did not read are counted
// now. It returns how many were dropped while waiting.
func (t *Tracker) Ready(startMS int64, throughID uint) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	t.ready, t.throughID, t.startMS = true, throughID, startMS
	for _, ev := range t.pending {
		if ev.ID == 0 || ev.ID > throughID {
			t.apply(ev)
		}
	}
	t.pending = nil
	return t.dropped
}

// IsReady says the replay is in.
func (t *Tracker) IsReady() bool {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.ready
}

// StartMS is when the memory begins (Unix ms): the replay's start.
func (t *Tracker) StartMS() int64 {
	t.mu.Lock()
	defer t.mu.Unlock()
	return t.startMS
}

func (t *Tracker) node(uuid string) uint32 {
	if id, ok := t.nodes[uuid]; ok {
		return id
	}
	id := uint32(len(t.nodeNames))
	t.nodes[uuid] = id
	t.nodeNames = append(t.nodeNames, uuid)
	return id
}

// slot is the bucket of a minute, emptied if it held an older one; nil for a
// minute older than the ring holds.
func (t *Tracker) slot(minute int64) *bucket {
	b := &t.ring[minute%ringMinutes]
	switch {
	case b.minute == minute:
	case b.minute < minute:
		*b = bucket{minute: minute, reasons: b.reasons[:0], cards: b.cards[:0], deps: b.deps[:0], nodeHits: b.nodeHits[:0]}
		if b.reasons == nil {
			b.reasons = make([]int64, reasonCount)
		} else {
			b.reasons = b.reasons[:reasonCount]
			clear(b.reasons)
		}
	default:
		return nil
	}
	return b
}

func (t *Tracker) apply(ev store.ObservedEvent) {
	at := ev.At.UnixMilli()
	if ev.Residency != nil {
		t.applyResidency(ev, at)
		return
	}
	b := t.slot(at / minuteMS)
	if b == nil {
		return
	}
	if ev.Counted {
		if i := store.ActivityReasonIndex(ev.Reason); i >= 0 {
			b.reasons[i]++
		}
	}
	first := len(b.deps)
	switch {
	case ev.Yield:
		b.deps = append(b.deps, depHit{uint32(ev.DeploymentID), hitYielded})
		if ev.ForDeployment != 0 && ev.ForDeployment != ev.DeploymentID {
			b.deps = append(b.deps, depHit{uint32(ev.ForDeployment), hitYieldedTo})
		}
	case ev.Wake:
		b.deps = append(b.deps, depHit{uint32(ev.DeploymentID), hitWake})
	default:
		return
	}
	// The event's nodes are those of its cards (sorted by node, so a node's
	// cards are next to each other); each hit is placed on each once.
	var last uint32
	for i, c := range ev.Cards {
		node := t.node(c.NodeUUID)
		b.cards = append(b.cards, cardHit{node, int32(c.GPU)})
		if i > 0 && node == last {
			continue
		}
		last = node
		for hit := first; hit < len(b.deps); hit++ {
			b.nodeHits = append(b.nodeHits, nodeHit{uint32(hit), node})
		}
	}
}

func (t *Tracker) applyResidency(ev store.ObservedEvent, at int64) {
	inst := t.instances[ev.InstanceUUID]
	if inst == nil {
		inst = &instance{dep: ev.DeploymentID}
		t.instances[ev.InstanceUUID] = inst
	}
	inst.m.Apply(*ev.Residency)
	inst.seen = max(inst.seen, at)
	state, held := inst.m.Label()
	awake := held && state == store.ResidencyAwake
	if awake == inst.awake && held == inst.held {
		if !held && ev.Residency.Code == store.EventInstanceRemoved {
			delete(t.instances, ev.InstanceUUID)
		}
		return
	}
	dep := t.deps[inst.dep]
	if dep == nil {
		dep = &deployment{}
		t.deps[inst.dep] = dep
	}
	if awake != inst.awake {
		dep.awake.change(delta(awake), at)
	}
	if held != inst.held {
		dep.held.change(delta(held), at)
	}
	inst.awake, inst.held = awake, held
	if !held && ev.Residency.Code == store.EventInstanceRemoved {
		delete(t.instances, ev.InstanceUUID)
	}
}

func delta(on bool) int {
	if on {
		return 1
	}
	return -1
}

// Forget ends, at at, every instance followed that live does not hold: one
// deleted without a removal event (it held no card) would otherwise stay
// awake for ever. live is every instance row's UUID as read at readAt; an
// instance heard of since (created after the read) is kept.
func (t *Tracker) Forget(live map[string]bool, readAt, at time.Time) int {
	t.mu.Lock()
	defer t.mu.Unlock()
	gone := 0
	for uuid, inst := range t.instances {
		if live[uuid] || inst.seen >= readAt.Add(-forgetMargin).UnixMilli() {
			continue
		}
		gone++
		t.applyResidency(store.ObservedEvent{At: at, DeploymentID: inst.dep, InstanceUUID: uuid,
			Residency: &store.ResidencyEvent{Code: store.EventInstanceRemoved, At: at}}, at.UnixMilli())
		delete(t.instances, uuid)
	}
	return gone
}

// Prune drops what ended before the span: the deployments' stretches and
// the deployments left with none. Buckets age out of the ring by
// themselves.
func (t *Tracker) Prune(now time.Time) {
	t.mu.Lock()
	defer t.mu.Unlock()
	cutoff := now.Add(-Span - time.Hour).UnixMilli()
	for id, d := range t.deps {
		d.awake.prune(cutoff)
		d.held.prune(cutoff)
		if d.held.count == 0 && len(d.held.closed) == 0 && d.awake.count == 0 && len(d.awake.closed) == 0 {
			delete(t.deps, id)
		}
	}
}
