package automation

import (
	"slices"
	"sort"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// Three time-sliced deployments share the cards. One holds them idle; one
// gave its cards up earlier and only waits to take them back; a request then
// waits for the third. The holder yields once its idle time is up, and it
// must yield for the request, though the resume has waited longer. Measured
// on an 8x H20 node before this ordering: the request waited behind a resume
// for the resumer's whole idle time as well, 67 s to its first token instead
// of about 8 s.
func TestARequestIsServedBeforeAResumeOfTheSameRank(t *testing.T) {
	w := newResidencyWorld()
	const holder, resumer, requested = 1, 2, 3
	for _, id := range []uint{holder, resumer, requested} {
		w.levels[id] = pol(yieldpolicy.TimeSlice)
	}
	w.asleep[resumer], w.asleep[requested] = true, true
	w.activity[holder] = Activity{LastRequest: t0}
	cards := Blocking{Holders: []Holder{{DeploymentID: holder, Policy: pol(yieldpolicy.TimeSlice), Instances: []string{"h-1"}}}}
	w.blocking[resumer], w.blocking[requested] = cards, cards
	e, c := newResidencyEvaluator(w, t0)
	e.res.awakeAt[holder] = t0
	e.res.preempted[resumer] = t0.Add(-time.Minute)
	w.waiters = []Waiter{{DeploymentID: requested, Policy: pol(yieldpolicy.TimeSlice), Since: t0.Add(5 * time.Second)}}

	for i := 1; i <= 12; i++ {
		c.t = t0.Add(time.Duration(i*5) * time.Second)
		e.residency(c.t)
	}
	if !slices.Equal(w.yields, []string{"1 abort=false"}) {
		t.Fatalf("yields %v, want the idle holder to yield once", w.yields)
	}
	if got := w.params[len(w.params)-1]["for_deployment"]; got != uint(requested) {
		t.Fatalf("the holder yielded for %v, want the deployment a request waits for (%d), not the resume (%d)", got, requested, resumer)
	}
	if got := w.eventsFor(holder); !slices.Equal(got, []string{"1 AUTO_SLEEP preempt/idle"}) {
		t.Fatalf("holder events = %v", got)
	}
}

// A busy holder whose turn ends yields for the waiting request too, not for
// a resume that has waited longer (the same H20 measurement: the request
// waited 133 s, about 35 s of it for a resumer's idle time).
func TestATurnEndsForTheRequestNotTheResume(t *testing.T) {
	w := newResidencyWorld()
	const holder, resumer, requested = 1, 2, 3
	for _, id := range []uint{holder, resumer, requested} {
		w.levels[id] = pol(yieldpolicy.TimeSlice)
	}
	w.asleep[resumer], w.asleep[requested] = true, true
	w.activity[holder] = Activity{InFlight: 1, LastRequest: t0}
	w.inFlight["h-1"] = 1
	cards := Blocking{Holders: []Holder{{DeploymentID: holder, Policy: pol(yieldpolicy.TimeSlice), Instances: []string{"h-1"}}}}
	w.blocking[resumer], w.blocking[requested] = cards, cards
	e, c := newResidencyEvaluator(w, t0)
	e.res.awakeAt[holder] = t0
	e.res.preempted[resumer] = t0.Add(-time.Minute)
	w.waiters = []Waiter{{DeploymentID: requested, Policy: pol(yieldpolicy.TimeSlice), Since: t0.Add(5 * time.Second)}}

	// Busy until its turn is served: no one gets its cards.
	c.t = t0.Add(time.Minute)
	e.residency(c.t)
	if len(w.yields)+len(w.drains) != 0 {
		t.Fatalf("the holder gave way mid-turn: yields %v drains %v", w.yields, w.drains)
	}
	// The turn ends: it drains for the request.
	c.t = t0.Add(2*time.Minute + wakeRepeat)
	e.residency(c.t)
	if !slices.Equal(w.drains, []string{"1 h-1 true"}) {
		t.Fatalf("drains %v, want the holder to drain at the end of its turn", w.drains)
	}
	// Its request finishes within the drain; it sleeps, for the request.
	w.activity[holder] = Activity{LastRequest: c.t}
	w.inFlight["h-1"] = 0
	c.t = c.t.Add(10 * time.Second)
	e.residency(c.t)
	if !slices.Equal(w.yields, []string{"1 abort=false"}) {
		t.Fatalf("yields %v, want the drained holder to sleep once", w.yields)
	}
	if got := w.params[len(w.params)-1]["for_deployment"]; got != uint(requested) {
		t.Fatalf("the turn ended for %v, want the deployment a request waits for (%d), not the resume (%d)", got, requested, resumer)
	}
	if got := w.eventsFor(holder); !slices.Equal(got, []string{"1 AUTO_SLEEP preempt/turn"}) {
		t.Fatalf("holder events = %v", got)
	}
}

// The order itself: rank first (a resume of a higher rank still goes before
// a lower rank's request), then what waits for someone before a resume, then
// the longest waiting.
func TestQueueOrder(t *testing.T) {
	at := func(s int) time.Time { return t0.Add(time.Duration(s) * time.Second) }
	queue := []waiting{
		{id: 1, policy: pol(yieldpolicy.TimeSlice), since: at(0), resume: true},
		{id: 2, policy: pol(yieldpolicy.TimeSlice), since: at(20)},
		{id: 3, policy: pol(yieldpolicy.Anytime), since: at(-60)},
		{id: 4, policy: pol(yieldpolicy.Idle), since: at(30), resume: true},
		{id: 5, policy: pol(yieldpolicy.TimeSlice), since: at(10), place: true},
		{id: 6, policy: pol(yieldpolicy.TimeSlice), since: at(-10), resume: true},
	}
	sort.SliceStable(queue, func(a, b int) bool { return queueBefore(queue[a], queue[b]) })
	var got []uint
	for _, w := range queue {
		got = append(got, w.id)
	}
	if want := []uint{4, 5, 2, 6, 1, 3}; !slices.Equal(got, want) {
		t.Fatalf("order %v, want %v (%v)", got, want, queueText(queue))
	}
}
