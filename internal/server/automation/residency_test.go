package automation

import (
	"fmt"
	"slices"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/servicelevel"
)

// residencyWorld is the arbiter's view of a few deployments on shared cards.
type residencyWorld struct {
	*fakeWorld
	levels    map[uint]string
	asleep    map[uint]bool // asleep by the platform's choice
	waiters   []Waiter
	blocking  map[uint]Blocking
	activity  map[uint]Activity
	fitsNow   map[uint]bool // Wake wakes it at once
	yields    []string
	drains    []string
	yieldErr  error
	wakeCalls []uint
}

func newResidencyWorld() *residencyWorld {
	return &residencyWorld{
		fakeWorld: newFakeWorld(), levels: map[uint]string{}, asleep: map[uint]bool{}, blocking: map[uint]Blocking{},
		activity: map[uint]Activity{}, fitsNow: map[uint]bool{},
	}
}

func (w *residencyWorld) Waiters() ([]Waiter, error) { return w.waiters, nil }
func (w *residencyWorld) Wakeable(id uint) (string, bool) {
	return w.levels[id], w.asleep[id]
}
func (w *residencyWorld) Blocking(id uint) (Blocking, error) { return w.blocking[id], nil }
func (w *residencyWorld) Activity(id uint) Activity          { return w.activity[id] }
func (w *residencyWorld) Yield(id uint, instances []string, abort bool) (int, error) {
	w.yields = append(w.yields, fmt.Sprintf("%d abort=%v", id, abort))
	if w.yieldErr != nil {
		return 0, w.yieldErr
	}
	w.asleep[id] = true
	return 1, nil
}
func (w *residencyWorld) Drain(id uint, on bool) {
	w.drains = append(w.drains, fmt.Sprintf("%d %v", id, on))
}
func (w *residencyWorld) Wake(id uint) (WakeResult, error) {
	w.wakeCalls = append(w.wakeCalls, id)
	if w.fitsNow[id] {
		w.asleep[id] = false
		return WakeResult{Woken: []string{fmt.Sprintf("i-%d", id)}}, nil
	}
	return WakeResult{Waiting: []string{fmt.Sprintf("i-%d", id)}}, nil
}

func newResidencyEvaluator(w *residencyWorld, start time.Time) (*Evaluator, *clock) {
	e, c := newEvaluator(w.fakeWorld, start)
	e.actions = w
	e.WithResidency(w, w)
	e.started = start
	return e, c
}

func (w *residencyWorld) eventsFor(id uint) []string {
	var out []string
	prefix := fmt.Sprintf("%d ", id)
	for i, ev := range w.events {
		if strings.HasPrefix(ev, prefix) {
			out = append(out, fmt.Sprintf("%s %v/%v", ev, w.params[i]["trigger"], w.params[i]["reason"]))
		}
	}
	return out
}

var t0 = time.Date(2026, 10, 28, 9, 0, 0, 0, time.UTC)

func TestARequestWakesADeploymentThatFits(t *testing.T) {
	w := newResidencyWorld()
	w.levels[1] = servicelevel.Resident
	w.asleep[1] = true
	w.fitsNow[1] = true
	w.waiters = []Waiter{{DeploymentID: 1, Level: servicelevel.Resident, Since: t0}}
	e, c := newResidencyEvaluator(w, t0)
	e.residency(c.t)
	if !slices.Equal(w.events, []string{"1 AUTO_WAKE"}) || w.params[0]["trigger"] != TriggerRequest {
		t.Fatalf("events = %v %v", w.events, w.params)
	}
	// The wake is in flight: the next passes leave it alone for a while.
	c.t = c.t.Add(5 * time.Second)
	e.residency(c.t)
	if len(w.wakeCalls) != 1 {
		t.Fatalf("woke again while the first wake was in flight: %v", w.wakeCalls)
	}
	c.t = c.t.Add(wakeRepeat)
	e.residency(c.t)
	if len(w.wakeCalls) != 2 {
		t.Fatalf("a request still waiting after the repeat time was not looked at: %v", w.wakeCalls)
	}
}

func TestOnlineTakesItsCardsFromOfflineAndOfflineResumesWhenOnlineIdles(t *testing.T) {
	w := newResidencyWorld()
	const online, offline = 1, 2
	w.levels[online], w.levels[offline] = servicelevel.Online, servicelevel.Offline
	w.asleep[online] = true
	w.activity[offline] = Activity{InFlight: 3, LastRequest: t0}
	w.waiters = []Waiter{{DeploymentID: online, Level: servicelevel.Online, Since: t0}}
	w.blocking[online] = Blocking{Holders: []Holder{{DeploymentID: offline, Level: servicelevel.Offline, Instances: []string{"off-1"}}}}
	e, c := newResidencyEvaluator(w, t0)
	e.residency(c.t)
	if !slices.Equal(w.yields, []string{"2 abort=true"}) {
		t.Fatalf("offline was not cut off at once for online: %v", w.yields)
	}
	if got := w.eventsFor(offline); !slices.Equal(got, []string{"2 AUTO_SLEEP preempt/rank"}) {
		t.Fatalf("offline events = %v", got)
	}

	// Online wakes (its wake waited for the room) and serves; offline wants
	// its cards back but online is busy.
	w.waiters = nil
	w.asleep[online] = false
	w.fitsNow[offline] = false
	w.blocking[offline] = Blocking{Holders: []Holder{{DeploymentID: online, Level: servicelevel.Online, Instances: []string{"on-1"}}}}
	w.activity[online] = Activity{InFlight: 1, LastRequest: c.t}
	c.t = c.t.Add(time.Minute)
	e.residency(c.t)
	if len(w.yields) != 1 {
		t.Fatalf("busy online was put to sleep for offline: %v", w.yields)
	}
	// Online idle, but not yet five minutes.
	w.activity[online] = Activity{LastRequest: c.t}
	c.t = c.t.Add(4 * time.Minute)
	e.residency(c.t)
	if len(w.yields) != 1 {
		t.Fatalf("online gave its cards up before its idle time: %v", w.yields)
	}
	c.t = c.t.Add(time.Minute)
	e.residency(c.t)
	if !slices.Equal(w.yields, []string{"2 abort=true", "1 abort=false"}) {
		t.Fatalf("idle online did not give way to offline resuming: %v", w.yields)
	}
	if got := w.eventsFor(online); !slices.Equal(got, []string{"1 AUTO_SLEEP preempt/idle"}) {
		t.Fatalf("online events = %v", got)
	}
	// Online is not preemptible: it does not try to resume on its own, only
	// when a request comes.
	if _, ok := e.res.preempted[online]; ok {
		t.Fatal("online was queued to resume without a request")
	}
	if _, ok := e.res.preempted[offline]; !ok {
		t.Fatal("offline left the resume queue before it woke")
	}
}

func TestSharedDeploymentsTakeTurnsAndDrain(t *testing.T) {
	w := newResidencyWorld()
	const a, b = 1, 2
	w.levels[a], w.levels[b] = servicelevel.Shared, servicelevel.Shared
	w.asleep[b] = true
	w.activity[a] = Activity{InFlight: 2, LastRequest: t0}
	w.blocking[b] = Blocking{Holders: []Holder{{DeploymentID: a, Level: servicelevel.Shared, Instances: []string{"a-1"}}}}
	e, c := newResidencyEvaluator(w, t0)
	e.res.awakeAt[a] = t0
	w.waiters = []Waiter{{DeploymentID: b, Level: servicelevel.Shared, Since: t0}}

	c.t = t0.Add(time.Minute)
	e.residency(c.t)
	if len(w.yields)+len(w.drains) != 0 {
		t.Fatalf("a took b's cards before its turn: yields %v drains %v", w.yields, w.drains)
	}
	c.t = t0.Add(2*time.Minute + wakeRepeat)
	e.residency(c.t)
	if !slices.Equal(w.drains, []string{"1 true"}) || len(w.yields) != 0 {
		t.Fatalf("a busy shared holder must drain first: drains %v yields %v", w.drains, w.yields)
	}
	// Its requests finish within the drain: it sleeps without cutting any off.
	w.activity[a] = Activity{LastRequest: c.t}
	c.t = c.t.Add(10 * time.Second)
	e.residency(c.t)
	if !slices.Equal(w.yields, []string{"1 abort=false"}) || !slices.Equal(w.drains, []string{"1 true", "1 false"}) {
		t.Fatalf("after draining: yields %v drains %v", w.yields, w.drains)
	}
	if got := w.eventsFor(a); !slices.Equal(got, []string{"1 AUTO_SLEEP preempt/turn"}) {
		t.Fatalf("a's events = %v", got)
	}
}

func TestADrainThatRunsOverCutsTheRestOff(t *testing.T) {
	w := newResidencyWorld()
	const a, b = 1, 2
	w.levels[a], w.levels[b] = servicelevel.Shared, servicelevel.Online
	w.asleep[b] = true
	w.activity[a] = Activity{InFlight: 1, LastRequest: t0}
	w.blocking[b] = Blocking{Holders: []Holder{{DeploymentID: a, Level: servicelevel.Shared, Instances: []string{"a-1"}}}}
	w.waiters = []Waiter{{DeploymentID: b, Level: servicelevel.Online, Since: t0}}
	e, c := newResidencyEvaluator(w, t0)
	e.residency(c.t)
	if !slices.Equal(w.drains, []string{"1 true"}) {
		t.Fatalf("drains = %v", w.drains)
	}
	c.t = c.t.Add(29 * time.Second)
	e.residency(c.t)
	if len(w.yields) != 0 {
		t.Fatalf("cut off before the drain time: %v", w.yields)
	}
	c.t = c.t.Add(time.Second)
	e.residency(c.t)
	if !slices.Equal(w.yields, []string{"1 abort=true"}) {
		t.Fatalf("an overrun drain was not cut off: %v", w.yields)
	}
}

func TestNoOneYieldsUnlessEveryHolderMay(t *testing.T) {
	for _, tc := range []struct {
		name     string
		blocking Blocking
	}{
		{"a resident holder", Blocking{Holders: []Holder{{DeploymentID: 2, Level: servicelevel.Offline}, {DeploymentID: 3, Level: servicelevel.Resident}}}},
		{"a busy online holder", Blocking{Holders: []Holder{{DeploymentID: 2, Level: servicelevel.Offline}, {DeploymentID: 3, Level: servicelevel.Online}}}},
		{"an occupant nothing can move", Blocking{Holders: []Holder{{DeploymentID: 2, Level: servicelevel.Offline}}, Blocked: true}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			w := newResidencyWorld()
			w.levels[1] = servicelevel.Shared
			w.asleep[1] = true
			w.activity[3] = Activity{InFlight: 1}
			w.blocking[1] = tc.blocking
			w.waiters = []Waiter{{DeploymentID: 1, Level: servicelevel.Shared, Since: t0}}
			e, c := newResidencyEvaluator(w, t0)
			c.t = t0.Add(time.Hour)
			e.residency(c.t)
			if len(w.yields)+len(w.drains) != 0 {
				t.Fatalf("yields %v drains %v", w.yields, w.drains)
			}
		})
	}
}

func TestHigherRankWaitersAreServedFirstAndAHolderYieldsOnce(t *testing.T) {
	w := newResidencyWorld()
	// Offline 3 holds the cards both shared 1 (waiting longer) and online 2
	// want: online goes first, and the holder is not put to sleep twice.
	w.levels[1], w.levels[2], w.levels[3] = servicelevel.Shared, servicelevel.Online, servicelevel.Offline
	w.asleep[1], w.asleep[2] = true, true
	holder := Blocking{Holders: []Holder{{DeploymentID: 3, Level: servicelevel.Offline, Instances: []string{"off"}}}}
	w.blocking[1], w.blocking[2] = holder, holder
	w.waiters = []Waiter{{DeploymentID: 1, Level: servicelevel.Shared, Since: t0}, {DeploymentID: 2, Level: servicelevel.Online, Since: t0.Add(time.Minute)}}
	w.activity[3] = Activity{InFlight: 4}
	e, c := newResidencyEvaluator(w, t0)
	c.t = t0.Add(2 * time.Minute)
	e.residency(c.t)
	if !slices.Equal(w.wakeCalls, []uint{2, 1}) {
		t.Fatalf("wake order = %v, want online first", w.wakeCalls)
	}
	if !slices.Equal(w.yields, []string{"3 abort=true"}) {
		t.Fatalf("yields = %v", w.yields)
	}
	if w.params[0]["for_deployment"] != uint(2) {
		t.Fatalf("holder yielded for %v, want online", w.params[0]["for_deployment"])
	}
}

func TestAFailedYieldIsRecorded(t *testing.T) {
	w := newResidencyWorld()
	w.levels[1], w.levels[2] = servicelevel.Online, servicelevel.Offline
	w.asleep[1] = true
	w.yieldErr = fmt.Errorf("agent unreachable")
	w.blocking[1] = Blocking{Holders: []Holder{{DeploymentID: 2, Level: servicelevel.Offline}}}
	w.waiters = []Waiter{{DeploymentID: 1, Level: servicelevel.Online, Since: t0}}
	e, c := newResidencyEvaluator(w, t0)
	e.residency(c.t)
	if got := w.eventsFor(2); len(got) != 1 || w.params[0]["action"] != "sleep" || w.params[0]["reason"] != "agent unreachable" {
		t.Fatalf("events = %v params %v", got, w.params)
	}
	if _, ok := e.res.preempted[2]; ok {
		t.Fatal("a holder that did not sleep was queued to resume")
	}
}

func TestOffWithoutResidency(t *testing.T) {
	w := newFakeWorld()
	e, c := newEvaluator(w, t0)
	e.Demand(1)
	e.residency(c.t)
	if len(w.orders) != 0 {
		t.Fatalf("orders = %v", w.orders)
	}
}

// A holder that gave its cards up does not take them straight back while the
// one it gave them to is still waking: that would undo the yield.
func TestAYieldedHolderLeavesTheCardsToTheWaiterWhileItWakes(t *testing.T) {
	w := newResidencyWorld()
	const online, offline = 1, 2
	w.levels[online], w.levels[offline] = servicelevel.Online, servicelevel.Offline
	w.asleep[online] = true
	w.waiters = []Waiter{{DeploymentID: online, Level: servicelevel.Online, Since: t0}}
	w.blocking[online] = Blocking{Holders: []Holder{{DeploymentID: offline, Level: servicelevel.Offline, Instances: []string{"off"}}}}
	e, c := newResidencyEvaluator(w, t0)
	e.residency(c.t)
	if !slices.Equal(w.yields, []string{"2 abort=true"}) {
		t.Fatalf("yields = %v", w.yields)
	}
	// The card is free now, and online's wake is still on its way: offline
	// would fit, but must not take it back.
	w.fitsNow[offline] = true
	c.t = c.t.Add(5 * time.Second)
	e.residency(c.t)
	if slices.Contains(w.wakeCalls, offline) {
		t.Fatalf("offline took the cards back while online woke: %v", w.wakeCalls)
	}
	// Online is awake: offline may try again (and finds online holding).
	w.asleep[online] = false
	w.waiters = nil
	c.t = c.t.Add(5 * time.Second)
	e.residency(c.t)
	if !slices.Contains(w.wakeCalls, offline) {
		t.Fatalf("offline never tried to resume after online woke: %v", w.wakeCalls)
	}
}

func TestAYieldGuardDoesNotOutliveItsTime(t *testing.T) {
	w := newResidencyWorld()
	w.levels[1], w.levels[2] = servicelevel.Online, servicelevel.Offline
	w.asleep[1], w.asleep[2] = true, true
	e, c := newResidencyEvaluator(w, t0)
	e.res.preempted[2] = t0
	e.res.yieldedFor[2] = yielded{to: 1, at: t0}
	w.fitsNow[2] = true
	c.t = t0.Add(yieldGuard)
	e.residency(c.t)
	if !slices.Contains(w.wakeCalls, 2) {
		t.Fatalf("a holder stayed asleep past the guard for a waiter that never woke: %v", w.wakeCalls)
	}
}

// A wake the arbiter ordered that had to wait for a holder to yield is
// recorded with the arbiter's reason when it lands, not as a plain waiting
// wake.
func TestAWakeThatWaitedForAYieldKeepsItsTrigger(t *testing.T) {
	w := newResidencyWorld()
	w.levels[1], w.levels[2] = servicelevel.Online, servicelevel.Offline
	w.asleep[1] = true
	w.activity[2] = Activity{InFlight: 1}
	w.waiters = []Waiter{{DeploymentID: 1, Level: servicelevel.Online, Since: t0}}
	w.blocking[1] = Blocking{Holders: []Holder{{DeploymentID: 2, Level: servicelevel.Offline, Instances: []string{"off"}}}}
	e, c := newResidencyEvaluator(w, t0)
	e.residency(c.t)
	w.waits = []WaitingWake{{DeploymentID: 1, Instances: []string{"i-1"}, Since: t0}}
	w.fits[1] = []string{"i-1"}
	c.t = c.t.Add(2 * time.Second)
	e.waitingWakes(c.t)
	last := len(w.events) - 1
	if w.events[last] != "1 AUTO_WAKE" || w.params[last]["trigger"] != TriggerRequest {
		t.Fatalf("last event %s %v, want AUTO_WAKE trigger request", w.events[last], w.params[last])
	}
}
