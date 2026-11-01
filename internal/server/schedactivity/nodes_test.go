package schedactivity

import (
	"fmt"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/store"
)

// A deployment's yields, yields to it and wakes on some nodes are those of
// the events whose cards are there: an event over cards on two nodes counts
// on each, and once for a read naming both; an event with no card counts in
// the fleet's figures only.
func TestDeploymentsOnNodes(t *testing.T) {
	tr := NewTracker()
	tr.Ready(base.UnixMilli(), 0)
	a0, a1, b0 := store.EventCard{NodeUUID: "a", GPU: 0}, store.EventCard{NodeUUID: "a", GPU: 1}, store.EventCard{NodeUUID: "b", GPU: 0}
	tr.Observe(yield(1, 7, 8, at(time.Minute), store.ActivityTimesliceTurn, a0, a1))   // two cards of node a
	tr.Observe(yield(2, 7, 8, at(2*time.Minute), store.ActivityTimesliceTurn, a1, b0)) // one card each of a and b
	tr.Observe(yield(3, 7, 9, at(2*time.Minute), store.ActivityIdleYield, b0))
	tr.Observe(store.ObservedEvent{ID: 4, At: at(3 * time.Minute), DeploymentID: 7, Wake: true, Cards: []store.EventCard{b0}})
	tr.Observe(yield(5, 7, 8, at(4*time.Minute), store.ActivityRankPreemption)) // no card known
	from, to := base, at(time.Hour)
	for _, c := range []struct {
		nodes []string
		want  map[uint]store.ResidencySwitches
	}{
		{[]string{"a"}, map[uint]store.ResidencySwitches{7: {Yielded: 2}, 8: {YieldedTo: 2}}},
		{[]string{"b"}, map[uint]store.ResidencySwitches{7: {Yielded: 2, Wakes: 1}, 8: {YieldedTo: 1}, 9: {YieldedTo: 1}}},
		{[]string{"a", "b"}, map[uint]store.ResidencySwitches{7: {Yielded: 3, Wakes: 1}, 8: {YieldedTo: 2}, 9: {YieldedTo: 1}}},
		{[]string{"unknown"}, map[uint]store.ResidencySwitches{}},
	} {
		if got := tr.DeploymentsOnNodes(c.nodes, from, to, nil); fmt.Sprint(got) != fmt.Sprint(c.want) {
			t.Errorf("%v = %v, want %v", c.nodes, got, c.want)
		}
	}
	if got := tr.DeploymentsOnNodes([]string{"a", "b"}, from, to, map[uint]bool{9: true}); fmt.Sprint(got) != fmt.Sprint(map[uint]store.ResidencySwitches{9: {YieldedTo: 1}}) {
		t.Errorf("limited to 9 = %v", got)
	}
	// Outside the range nothing counts.
	if got := tr.DeploymentsOnNodes([]string{"a", "b"}, at(10*time.Minute), to, nil); len(got) != 0 {
		t.Errorf("after the events = %v", got)
	}
	// The fleet's figures count every event, the one with no card too.
	if fleet := tr.Deployments(from, to, nil); fleet[7].Yielded != 4 || fleet[7].Wakes != 1 || fleet[8].YieldedTo != 3 {
		t.Errorf("fleet = %+v", fleet)
	}
	// A minute reused by a later one forgets its node hits with the rest.
	later := at(time.Duration(ringMinutes)*time.Minute + time.Minute)
	tr.Observe(yield(6, 7, 8, later, store.ActivityTimesliceTurn, b0))
	if got := tr.DeploymentsOnNodes([]string{"a", "b"}, later.Add(-time.Minute), later.Add(time.Minute), nil); fmt.Sprint(got) != fmt.Sprint(map[uint]store.ResidencySwitches{7: {Yielded: 1}, 8: {YieldedTo: 1}}) {
		t.Errorf("reused minute = %v", got)
	}
}
