package automation

import (
	"slices"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/servicelevel"
)

func TestTheEvaluatorTicksEveryTenSeconds(t *testing.T) {
	if DefaultInterval != 10*time.Second {
		t.Fatalf("DefaultInterval = %v", DefaultInterval)
	}
	if e := New(newFakeWorld(), newFakeWorld(), nil); e.interval != 10*time.Second {
		t.Fatalf("interval = %v", e.interval)
	}
}

// holderWorld is a holder (1) awake with requests running and a waiting
// higher-ranked deployment (2) that has to wait for its cards.
func holderWorld(holder servicelevel.Policy, inFlight int) (*residencyWorld, *Evaluator, *clock) {
	w := newResidencyWorld()
	w.levels[1], w.levels[2] = holder, pol(servicelevel.Idle)
	w.asleep[2] = true
	w.activity[1] = Activity{InFlight: inFlight, LastRequest: t0}
	w.blocking[2] = Blocking{Holders: []Holder{{DeploymentID: 1, Policy: holder, Instances: []string{"h-1"}}}}
	w.waiters = []Waiter{{DeploymentID: 2, Policy: pol(servicelevel.Idle), Since: t0}}
	e, c := newResidencyEvaluator(w, t0)
	return w, e, c
}

func TestUnfinishedRequestsAreHandledAsThePolicySays(t *testing.T) {
	waitFor := func(sec int) servicelevel.Policy {
		p := pol(servicelevel.TimeSlice)
		p.UnfinishedWaitSec = sec
		return p
	}
	abortShared := pol(servicelevel.TimeSlice)
	abortShared.Unfinished, abortShared.UnfinishedWaitSec = servicelevel.UnfinishedAbort, 0
	waitOffline := pol(servicelevel.Anytime)
	waitOffline.Unfinished, waitOffline.UnfinishedWaitSec = servicelevel.UnfinishedWait, 90

	t.Run("abort cuts them off at once, even on a timeslice deployment", func(t *testing.T) {
		w, e, c := holderWorld(abortShared, 4)
		e.residency(c.t)
		if !slices.Equal(w.yields, []string{"1 abort=true"}) || len(w.drains) != 0 {
			t.Fatalf("yields %v drains %v", w.yields, w.drains)
		}
	})
	t.Run("abort on an anytime deployment is the preset", func(t *testing.T) {
		w, e, c := holderWorld(pol(servicelevel.Anytime), 4)
		e.residency(c.t)
		if !slices.Equal(w.yields, []string{"1 abort=true"}) {
			t.Fatalf("yields %v", w.yields)
		}
	})
	t.Run("wait drains for the time the policy gives and then cuts off", func(t *testing.T) {
		w, e, c := holderWorld(waitFor(45), 4)
		e.residency(c.t)
		if !slices.Equal(w.drains, []string{"1 true"}) || len(w.yields) != 0 {
			t.Fatalf("drains %v yields %v", w.drains, w.yields)
		}
		c.t = c.t.Add(44 * time.Second)
		e.residency(c.t)
		if len(w.yields) != 0 {
			t.Fatalf("cut off before the 45 s: %v", w.yields)
		}
		c.t = c.t.Add(time.Second)
		e.residency(c.t)
		if !slices.Equal(w.yields, []string{"1 abort=true"}) {
			t.Fatalf("not cut off at 45 s: %v", w.yields)
		}
	})
	t.Run("wait on an anytime deployment drains too", func(t *testing.T) {
		w, e, c := holderWorld(waitOffline, 2)
		e.residency(c.t)
		c.t = c.t.Add(89 * time.Second)
		e.residency(c.t)
		if !slices.Equal(w.drains, []string{"1 true"}) || len(w.yields) != 0 {
			t.Fatalf("drains %v yields %v", w.drains, w.yields)
		}
		c.t = c.t.Add(time.Second)
		e.residency(c.t)
		if !slices.Equal(w.yields, []string{"1 abort=true"}) {
			t.Fatalf("yields %v", w.yields)
		}
	})
	t.Run("a wait that ends in time sleeps without cutting anything off", func(t *testing.T) {
		w, e, c := holderWorld(waitFor(45), 4)
		e.residency(c.t)
		w.activity[1] = Activity{LastRequest: c.t}
		c.t = c.t.Add(5 * time.Second)
		e.residency(c.t)
		if !slices.Equal(w.yields, []string{"1 abort=false"}) || !slices.Equal(w.drains, []string{"1 true", "1 false"}) {
			t.Fatalf("yields %v drains %v", w.yields, w.drains)
		}
	})
	t.Run("with nothing running a wait sleeps at once", func(t *testing.T) {
		w, e, c := holderWorld(waitFor(45), 0)
		e.residency(c.t)
		if !slices.Equal(w.yields, []string{"1 abort=false"}) || len(w.drains) != 0 {
			t.Fatalf("yields %v drains %v", w.yields, w.drains)
		}
	})
}

// idleHolderWorld is an idle holder of the policy (1) with a waiter (2) of
// the policy, looked at once, idleFor into the holder's idleness.
func idleHolderWorld(holder, waiter servicelevel.Policy, idleFor time.Duration) *residencyWorld {
	w := newResidencyWorld()
	w.levels[1], w.levels[2] = holder, waiter
	w.asleep[2] = true
	w.activity[1] = Activity{LastRequest: t0}
	w.blocking[2] = Blocking{Holders: []Holder{{DeploymentID: 1, Policy: holder, Instances: []string{"a"}}}}
	w.waiters = []Waiter{{DeploymentID: 2, Policy: waiter, Since: t0}}
	e, c := newResidencyEvaluator(w, t0)
	c.t = t0.Add(idleFor)
	e.residency(c.t)
	return w
}

// A custom idle time moves when an idle deployment gives its cards up.
func TestAnIdleDeploymentGivesWayAfterItsOwnIdleTime(t *testing.T) {
	custom := pol(servicelevel.Idle)
	custom.IdleSec = 30
	offline := pol(servicelevel.Anytime)
	if w := idleHolderWorld(custom, offline, 29*time.Second); len(w.yields) != 0 {
		t.Fatalf("gave way before its 30 s: %v", w.yields)
	}
	if w := idleHolderWorld(custom, offline, 30*time.Second); !slices.Equal(w.yields, []string{"1 abort=false"}) {
		t.Fatalf("did not give way at 30 s: %v", w.yields)
	}
}

// An anytime holder yields to an equal waiting once idle for its idle time.
func TestEqualAnytimeDeploymentsYieldAfterTheirIdleTime(t *testing.T) {
	holder := pol(servicelevel.Anytime)
	holder.IdleSec = 45
	if w := idleHolderWorld(holder, pol(servicelevel.Anytime), 44*time.Second); len(w.yields) != 0 {
		t.Fatalf("yielded to an equal before it was idle 45 s: %v", w.yields)
	}
	if w := idleHolderWorld(holder, pol(servicelevel.Anytime), 45*time.Second); !slices.Equal(w.yields, []string{"1 abort=false"}) {
		t.Fatalf("yields %v", w.yields)
	}
}
