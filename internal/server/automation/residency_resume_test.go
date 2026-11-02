package automation

import (
	"slices"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// Two time-sliced deployments share the cards. A keeps serving requests; B
// gave its cards up earlier and has no request. A must not stop for B: a
// resume answers no one. Before, A drained at the end of every turn and B
// then idled its cards away until it gave them back - on an 8x H20 node the
// two swapped every 18 s with no request for either.
func TestABusyHolderDoesNotStopForAResume(t *testing.T) {
	w, e, c := sharedPair(t)
	const a, b = 1, 2
	w.activity[a] = Activity{InFlight: 1, LastRequest: t0}
	w.inFlight["a-1"] = 1

	for i := 1; i <= 20; i++ {
		c.t = t0.Add(time.Duration(i) * 30 * time.Second)
		w.activity[a] = Activity{InFlight: 1, LastRequest: c.t}
		e.residency(c.t)
	}
	if len(w.yields)+len(w.drains)+len(w.wakeCalls) != 0 {
		t.Fatalf("A stopped for B's resume: yields %v drains %v wakes %v", w.yields, w.drains, w.wakeCalls)
	}

	// A request now waits for B: A drains at the end of its turn, as two
	// time-sliced deployments with requests take turns.
	w.waiters = []Waiter{{DeploymentID: b, Policy: pol(yieldpolicy.TimeSlice), Since: c.t}}
	c.t = c.t.Add(time.Second)
	e.residency(c.t)
	if !slices.Equal(w.drains, []string{"1 a-1 true"}) {
		t.Fatalf("drains %v, want A to drain for the request for B", w.drains)
	}
}

// An idle holder keeps its cards too while the other only waits to resume:
// with no request for either, nothing switches.
func TestAnIdleHolderDoesNotStopForAResume(t *testing.T) {
	w, e, c := sharedPair(t)
	const a = 1
	w.activity[a] = Activity{LastRequest: t0}
	for i := 1; i <= 20; i++ {
		c.t = t0.Add(time.Duration(i) * 30 * time.Second)
		e.residency(c.t)
	}
	if len(w.yields)+len(w.drains)+len(w.wakeCalls) != 0 {
		t.Fatalf("idle A stopped for B's resume: yields %v drains %v wakes %v", w.yields, w.drains, w.wakeCalls)
	}
	if _, ok := e.res.preempted[2]; !ok {
		t.Fatal("B left the resume list without taking its cards back")
	}
}

// Once no one holds B's cards, B takes them back without a request, and
// leaves the resume list once it wakes.
func TestAResumeTakesFreeCards(t *testing.T) {
	w, e, c := sharedPair(t)
	const b = 2
	w.blocking[b] = Blocking{}
	w.fitsNow[b] = true
	c.t = t0.Add(10 * time.Second)
	e.residency(c.t)
	if !slices.Equal(w.wakeCalls, []uint{b}) || len(w.yields) != 0 {
		t.Fatalf("wakes %v yields %v, want B woken on its free cards", w.wakeCalls, w.yields)
	}
	if got := w.eventsFor(b); !slices.Equal(got, []string{"2 AUTO_WAKE resume/<nil>"}) {
		t.Fatalf("B events = %v", got)
	}
}

// sharedPair is two time-sliced deployments on the same cards: A (1) awake
// holding them, B (2) asleep, put to sleep for A a minute ago, with no request.
func sharedPair(t *testing.T) (*residencyWorld, *Evaluator, *clock) {
	t.Helper()
	w := newResidencyWorld()
	const a, b = 1, 2
	w.levels[a], w.levels[b] = pol(yieldpolicy.TimeSlice), pol(yieldpolicy.TimeSlice)
	w.asleep[b] = true
	w.blocking[b] = Blocking{Holders: []Holder{{DeploymentID: a, Policy: pol(yieldpolicy.TimeSlice), Instances: []string{"a-1"}}}}
	e, c := newResidencyEvaluator(w, t0)
	e.res.awakeAt[a] = t0
	e.res.preempted[b] = t0.Add(-time.Minute)
	return w, e, c
}
