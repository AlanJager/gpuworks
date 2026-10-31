package automation

import (
	"slices"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// A holder with two replicas gives up the one on the waiter's cards. What it
// drains is that replica's requests, not its deployment's: the sibling goes
// on serving, and its traffic neither delays the sleep nor has it cut off.
func TestAReplicaDrainsItsOwnRequestsWhileItsSiblingServes(t *testing.T) {
	wait := pol(yieldpolicy.TimeSlice)
	wait.UnfinishedWaitSec = 45
	world := func() (*residencyWorld, *Evaluator, *clock) {
		w := newResidencyWorld()
		w.levels[1], w.levels[2] = wait, pol(yieldpolicy.Idle)
		w.asleep[2] = true
		// The deployment is busy: its sibling a-2 serves three requests.
		w.activity[1] = Activity{InFlight: 3, LastRequest: t0}
		w.inFlight["a-2"] = 3
		w.blocking[2] = Blocking{Holders: []Holder{{DeploymentID: 1, Policy: wait, Instances: []string{"a-1"}}}}
		w.waiters = []Waiter{{DeploymentID: 2, Policy: pol(yieldpolicy.Idle), Since: t0}}
		e, c := newResidencyEvaluator(w, t0)
		return w, e, c
	}

	t.Run("a replica with nothing running sleeps at once", func(t *testing.T) {
		w, e, c := world()
		e.residency(c.t)
		if !slices.Equal(w.yields, []string{"1 abort=false"}) || len(w.drains) != 0 {
			t.Fatalf("yields %v drains %v", w.yields, w.drains)
		}
	})
	t.Run("a busy replica drains alone and sleeps once its own requests end", func(t *testing.T) {
		w, e, c := world()
		w.inFlight["a-1"] = 1
		e.residency(c.t)
		if !slices.Equal(w.drains, []string{"1 a-1 true"}) || len(w.yields) != 0 {
			t.Fatalf("drains %v yields %v", w.drains, w.yields)
		}
		// Its request ends; the sibling's go on.
		w.inFlight["a-1"] = 0
		c.t = c.t.Add(5 * time.Second)
		e.residency(c.t)
		if !slices.Equal(w.yields, []string{"1 abort=false"}) || !slices.Equal(w.drains, []string{"1 a-1 true", "1 a-1 false"}) {
			t.Fatalf("yields %v drains %v", w.yields, w.drains)
		}
	})
}
