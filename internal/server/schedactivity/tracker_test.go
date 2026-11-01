package schedactivity

import (
	"fmt"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

var base = time.Date(2026, 10, 31, 8, 0, 0, 0, time.UTC)

func at(d time.Duration) time.Time { return base.Add(d) }

func yield(id uint, dep, forDep uint, when time.Time, reason store.ActivityReason, cards ...store.EventCard) store.ObservedEvent {
	return store.ObservedEvent{ID: id, At: when, DeploymentID: dep, Reason: reason, Counted: true, Yield: true, ForDeployment: forDep, Cards: cards}
}

func status(id uint, inst string, dep uint, when time.Time, state, phase string) store.ObservedEvent {
	return store.ObservedEvent{ID: id, At: when, DeploymentID: dep, InstanceUUID: inst,
		Residency: &store.ResidencyEvent{ID: id, Code: "INSTANCE_STATUS_UPDATED", At: when, State: state, Phase: phase}}
}

func sleepResult(id uint, inst string, dep uint, when time.Time, level int) store.ObservedEvent {
	return store.ObservedEvent{ID: id, At: when, DeploymentID: dep, InstanceUUID: inst,
		Residency: &store.ResidencyEvent{ID: id, Code: "INSTANCE_SLEEP_RESULT", At: when, Result: "applied", Level: level}}
}

func removed(id uint, inst string, dep uint, when time.Time) store.ObservedEvent {
	return store.ObservedEvent{ID: id, At: when, DeploymentID: dep, InstanceUUID: inst,
		Residency: &store.ResidencyEvent{ID: id, Code: store.EventInstanceRemoved, At: when}}
}

// Events observed while the replay runs wait for it; those it read are not
// counted twice, and the rest are counted once it is in.
func TestObservedEventsWaitForTheReplay(t *testing.T) {
	tr := NewTracker()
	card := store.EventCard{NodeUUID: "n1", GPU: 0}
	tr.Observe(yield(5, 1, 2, at(time.Minute), store.ActivityRankPreemption, card)) // the replay reads this one too
	tr.Observe(yield(9, 1, 2, at(2*time.Minute), store.ActivityIdleYield, card))    // after the replay's last id
	tr.Replayed([]store.ObservedEvent{yield(5, 1, 2, at(time.Minute), store.ActivityRankPreemption, card)})
	if tr.IsReady() {
		t.Fatal("ready before Ready")
	}
	if dropped := tr.Ready(base.UnixMilli(), 7); dropped != 0 {
		t.Fatalf("dropped %d", dropped)
	}
	tr.Observe(yield(6, 1, 2, at(3*time.Minute), store.ActivityRankPreemption, card)) // read by the replay
	tr.Observe(yield(10, 1, 2, at(3*time.Minute), store.ActivityTimesliceTurn, card))
	out := [][]int64{make([]int64, reasonCount)}
	tr.AddReasons(base.UnixMilli(), at(time.Hour).UnixMilli(), base.UnixMilli(), hourMS, out)
	if fmt.Sprint(out[0]) != "[1 1 1 0 0 0 0 0]" {
		t.Fatalf("reasons = %v, want one each of idle, rank, turn", out[0])
	}
	cards := map[store.EventCard]int64{}
	tr.AddCards(base.UnixMilli(), at(time.Hour).UnixMilli(), cards)
	if cards[card] != 3 {
		t.Fatalf("card switches = %v", cards)
	}
	deps := tr.Deployments(base, at(time.Hour), nil)
	if deps[1].Yielded != 3 || deps[2].YieldedTo != 3 || len(deps) != 2 {
		t.Fatalf("deployments = %+v", deps)
	}
	if limited := tr.Deployments(base, at(time.Hour), map[uint]bool{2: true}); len(limited) != 1 || limited[2].YieldedTo != 3 {
		t.Fatalf("limited = %+v", limited)
	}
}

// A deployment is awake while any of its instances is, and holds cards
// while any holds them in any state; its figures over a window are clipped
// to it, an open stretch running to the window's end.
func TestAwakeAndHeldTimeOfADeployment(t *testing.T) {
	tr := NewTracker()
	tr.Ready(base.UnixMilli(), 0)
	// a-1 starts at 0, runs at +10m, sleeps +60m..+61m (L1), awake again +120m.
	// a-2 runs +30m..+90m, then is removed.
	for _, ev := range []store.ObservedEvent{
		status(1, "a-1", 7, at(0), proto.InstStarting, ""),
		status(2, "a-1", 7, at(10*time.Minute), proto.InstRunning, ""),
		status(3, "a-2", 7, at(30*time.Minute), proto.InstRunning, ""),
		status(4, "a-1", 7, at(60*time.Minute), proto.InstRunning, proto.PhaseSleep),
		sleepResult(5, "a-1", 7, at(61*time.Minute), 1),
		status(6, "a-1", 7, at(61*time.Minute), proto.InstRunning, ""),
		removed(7, "a-2", 7, at(90*time.Minute)),
		status(8, "a-1", 7, at(119*time.Minute), proto.InstRunning, proto.PhaseSleep),
		sleepResult(9, "a-1", 7, at(120*time.Minute), 0),
		status(10, "a-1", 7, at(120*time.Minute), proto.InstRunning, ""),
	} {
		tr.Observe(ev)
	}
	d := tr.Deployments(base, at(3*time.Hour), nil)[7]
	// awake: a-1 10..60, a-2 30..90 -> 10..90, then 120..180.
	if d.AwakeMS != (80*time.Minute + 60*time.Minute).Milliseconds() {
		t.Fatalf("awake = %v", time.Duration(d.AwakeMS)*time.Millisecond)
	}
	if !d.HeldFrom.Equal(base) || !d.HeldTo.Equal(at(3*time.Hour)) {
		t.Fatalf("held %v..%v", d.HeldFrom, d.HeldTo)
	}
	clipped := tr.Deployments(at(70*time.Minute), at(100*time.Minute), nil)[7]
	if clipped.AwakeMS != (20 * time.Minute).Milliseconds() {
		t.Fatalf("clipped awake = %v", time.Duration(clipped.AwakeMS)*time.Millisecond)
	}
	// Pruning keeps what is within the span.
	tr.Prune(at(3 * time.Hour))
	if again := tr.Deployments(base, at(3*time.Hour), nil)[7]; again.AwakeMS != d.AwakeMS {
		t.Fatal("prune dropped a stretch inside the span")
	}
	// A removed instance is forgotten; one never seen holding is not kept.
	if _, ok := tr.instances["a-2"]; ok {
		t.Fatal("a removed instance is still tracked")
	}
}

// A bucket is reused for a later minute, never for an earlier one: an event
// older than the ring is dropped, not counted in a later minute.
func TestTheRingForgetsOldMinutes(t *testing.T) {
	tr := NewTracker()
	tr.Ready(base.UnixMilli(), 0)
	tr.Observe(yield(1, 1, 0, at(0), store.ActivityIdleYield))
	later := at(time.Duration(ringMinutes) * time.Minute)
	tr.Observe(yield(2, 1, 0, later, store.ActivityRankPreemption))
	tr.Observe(yield(3, 1, 0, at(0), store.ActivityTimesliceTurn)) // older than the ring now holds
	out := [][]int64{make([]int64, reasonCount)}
	tr.AddReasons(base.UnixMilli(), later.Add(time.Minute).UnixMilli(), base.UnixMilli(), later.Add(time.Minute).UnixMilli()-base.UnixMilli(), out)
	if fmt.Sprint(out[0]) != "[0 1 0 0 0 0 0 0]" {
		t.Fatalf("reasons = %v", out[0])
	}
}

// While waiting for the replay at most pendingCap events are held, the
// oldest dropped first.
func TestPendingIsBounded(t *testing.T) {
	tr := NewTracker()
	for i := 0; i < pendingCap+3; i++ {
		tr.Observe(yield(uint(i+1), 1, 0, at(0), store.ActivityIdleYield))
	}
	if dropped := tr.Ready(base.UnixMilli(), 0); dropped != 3 {
		t.Fatalf("dropped = %d, want 3", dropped)
	}
}

func TestSpansMergeAndClip(t *testing.T) {
	var s spans
	s.change(1, 10)
	s.change(1, 20)
	s.change(-1, 30)
	s.change(-1, 40) // 10..40
	s.change(1, 40)
	s.change(-1, 50) // merged: 10..50
	s.change(-1, 60) // below zero stays closed
	s.change(1, 70)  // open from 70
	if len(s.closed) != 1 || s.closed[0] != [2]int64{10, 50} {
		t.Fatalf("closed = %v", s.closed)
	}
	total, first, last := s.within(0, 100)
	if total != 40+30 || first != 10 || last != 100 {
		t.Fatalf("within = %d %d %d", total, first, last)
	}
	if total, _, _ := s.within(45, 75); total != 5+5 {
		t.Fatalf("clipped = %d", total)
	}
	s.prune(60)
	if len(s.closed) != 0 || s.count != 1 {
		t.Fatalf("pruned = %+v", s)
	}
}

// An instance deleted without a removal event (it held no card) stops being
// awake when the rows say it is gone; one heard of after the rows were read
// is kept.
func TestForgetEndsInstancesTheRowsNoLongerHold(t *testing.T) {
	tr := NewTracker()
	tr.Ready(base.UnixMilli(), 0)
	tr.Observe(status(1, "kept", 7, at(0), proto.InstRunning, ""))
	tr.Observe(status(2, "gone", 8, at(0), proto.InstRunning, ""))
	tr.Observe(status(4, "new", 9, at(2*time.Hour), proto.InstRunning, ""))
	if n := tr.Forget(map[string]bool{"kept": true}, at(2*time.Hour), at(time.Hour)); n != 1 {
		t.Fatalf("forgot %d, want the one gone (the new one was heard of after the read)", n)
	}
	deps := tr.Deployments(base, at(3*time.Hour), nil)
	if deps[7].AwakeMS != (3*time.Hour).Milliseconds() || deps[8].AwakeMS != time.Hour.Milliseconds() {
		t.Fatalf("awake %v and %v, want the kept instance's 3h and the gone one's 1h", time.Duration(deps[7].AwakeMS)*time.Millisecond, time.Duration(deps[8].AwakeMS)*time.Millisecond)
	}
	if _, ok := tr.instances["new"]; !ok {
		t.Fatal("an instance created after the read was forgotten")
	}
}
