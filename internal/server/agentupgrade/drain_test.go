package agentupgrade

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// drainFixture is two nodes serving deployment 7, which the gateway serves on
// n2 while n1 upgrades.
func drainFixture(t *testing.T) *fixture {
	t.Helper()
	f := newFixture(t, 2, true)
	f.servesOn(t, "n1", "inst-1", 7)
	f.servesOn(t, "n2", "inst-2", 7)
	f.gw.setServed(f.deploymentID(7), true)
	return f
}

// A node whose deployments are served elsewhere waits for its requests before
// its upgrade is dispatched: the gateway prefers the other replica, the
// console sees the stage, and nothing runs on the node when its agent is told
// to replace itself. The preference stays through the dispatch and ends with
// the attempt.
func TestUpgradeDrainsTheNodeBeforeItIsDispatched(t *testing.T) {
	f := drainFixture(t)
	r := f.create(t, "drain", []string{"n1"}, 0)
	var phases []string
	looks := 0
	f.gw.inflight = func([]string) int {
		looks++
		page, err := f.s.Items(context.Background(), r.Record.ID, 1, 20, "")
		if err != nil || len(page.Items) != 1 {
			t.Fatalf("items = %+v, %v", page, err)
		}
		phases = append(phases, page.Items[0].Record.Phase)
		if looks == 1 {
			if run, err := f.s.Get(context.Background(), r.Record.ID); err != nil || run.State != "running" || run.Counts.Active != 1 {
				t.Fatalf("run = %+v, %v; want the draining attempt shown as running", run, err)
			}
		}
		if looks < 3 {
			return 2
		}
		// The drain took a while: the dispatch, not the drain, is when the
		// attempt started.
		f.now = f.now.Add(time.Minute)
		return 0
	}
	drainStart := f.now
	dispatched := 0
	f.tr.onPush = func() {
		dispatched++
		if f.gw.avoiding() == 0 {
			t.Errorf("dispatched after the gateway stopped preferring the other replica")
		}
		if f.gw.lastServed() != 0 {
			t.Errorf("dispatched with %d request(s) still running on the node", f.gw.lastServed())
		}
	}
	f.tick(t)
	if dispatched != 1 || f.tr.count() != 1 {
		t.Fatalf("dispatches=%d calls=%v; want one upgrade dispatched", dispatched, f.tr.callList())
	}
	if looks < 3 {
		t.Fatalf("dispatched after %d look(s), before the node's requests finished", looks)
	}
	for _, phase := range phases {
		if phase != "draining" {
			t.Fatalf("stage while waiting = %q; want draining throughout %v", phase, phases)
		}
	}
	i := f.item(t, r.Record.ID, "n1")
	if i.Phase != "waiting_registration" || i.BaselineSessionID != "old-n1" || i.StartedAt == nil || i.DeadlineAt == nil {
		t.Fatalf("item = %+v; want it dispatched against the node's session, with a deadline", i)
	}
	if !i.StartedAt.Equal(drainStart.Add(time.Minute)) || !i.DeadlineAt.After(*i.StartedAt) {
		t.Fatalf("started %v deadline %v; want the start at the dispatch, after the drain began at %v", i.StartedAt, i.DeadlineAt, drainStart)
	}
	avoid, release := fmt.Sprintf("avoid:%d:inst-1", f.deploymentID(7)), fmt.Sprintf("release:%d:inst-1", f.deploymentID(7))
	if got := f.gw.preferenceCalls(); len(got) != 1 || got[0] != avoid {
		t.Fatalf("preference changes = %v; want inst-1 avoided before the dispatch", got)
	}
	f.tr.set("n1", Connection{SessionID: "new-n1", Version: "target", Ready: true, Caps: []string{proto.CapUpgradePush}})
	f.tick(t)
	if i := f.item(t, r.Record.ID, "n1"); i.Phase != "succeeded" || i.Error != "" {
		t.Fatalf("item = %+v; want the upgrade seen on the target version", i)
	}
	if got := f.gw.preferenceCalls(); len(got) != 2 || got[1] != release {
		t.Fatalf("preference changes = %v; want it released when the upgrade ended", got)
	}
}

// Requests that outlast the bound do not hold the upgrade back, and the
// attempt says the restart cut them, as a code.
func TestUpgradeProceedsAfterTheDrainBoundAndReportsWhatItCut(t *testing.T) {
	f := drainFixture(t)
	r := f.create(t, "slow", []string{"n1"}, 0)
	// Each look finds the requests still running and 25s gone by on the
	// service's clock: the minute's bound ends the wait on the third look.
	f.gw.inflight = func([]string) int {
		f.now = f.now.Add(25 * time.Second)
		return 3
	}
	f.tick(t)
	i := f.item(t, r.Record.ID, "n1")
	if f.tr.count() != 1 || i.Phase != "waiting_registration" {
		t.Fatalf("item = %+v calls=%v; want the upgrade dispatched at the bound", i, f.tr.callList())
	}
	if i.Reason != ReasonDrainIncomplete || i.Error != "" {
		t.Fatalf("item reason=%q error=%q; want the cut requests reported as a code, not as an error", i.Reason, i.Error)
	}
	if got := f.gw.looksTaken(); got != 3 {
		t.Fatalf("the wait looked %d time(s); want it to poll until the bound, on the service's clock", got)
	}
}

// The dispatch of a drained attempt fails to be recorded: nothing was sent,
// the preference ends at once, and the next pass returns the attempt to the
// queue and dispatches it.
func TestADrainedDispatchThatCannotBeRecordedIsRetried(t *testing.T) {
	f := drainFixture(t)
	r := f.create(t, "unrecorded", []string{"n1"}, 0)
	broken := false
	f.gw.inflight = func([]string) int {
		if !broken {
			broken = true
			if err := f.st.DB().Exec("ALTER TABLE agent_upgrade_items RENAME TO agent_upgrade_items_away").Error; err != nil {
				t.Fatal(err)
			}
		}
		return 0
	}
	if err := f.s.reconcile(context.Background()); err == nil {
		t.Fatal("reconcile hid the failed dispatch write")
	}
	if f.tr.count() != 0 || f.gw.avoiding() != 0 {
		t.Fatalf("calls=%v avoiding=%d; want nothing sent and nothing avoided", f.tr.callList(), f.gw.avoiding())
	}
	if err := f.st.DB().Exec("ALTER TABLE agent_upgrade_items_away RENAME TO agent_upgrade_items").Error; err != nil {
		t.Fatal(err)
	}
	if i := f.item(t, r.Record.ID, "n1"); i.Phase != "draining" {
		t.Fatalf("item = %+v; want the stage the failed write left", i)
	}
	f.tick(t)
	f.tick(t)
	if f.tr.count() != 1 {
		t.Fatalf("calls=%v; want the attempt requeued and dispatched", f.tr.callList())
	}
}

// A draining attempt this runner holds has no deadline yet: a pass that sees
// it does not record it as having missed one.
func TestAHeldDrainIsNotExpired(t *testing.T) {
	f := drainFixture(t)
	r := f.create(t, "held", []string{"n1"}, 0)
	item, err := f.st.AgentUpgradeItem(r.Record.ID, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := f.st.ClaimAgentUpgradeDrain(item.ID, f.now, f.s.heldSince()); err != nil || !claimed {
		t.Fatalf("claim the drain: %v, %v", claimed, err)
	}
	f.s.holds.take(item.ID, f.gw, drainSet{f.deploymentID(7): {"inst-1"}})
	f.now = f.now.Add(time.Hour)
	f.tick(t)
	if i := f.item(t, r.Record.ID, "n1"); i.Phase != "draining" {
		t.Fatalf("item = %+v; want the held drain left alone", i)
	}
	if !f.s.holds.held(item.ID) || f.gw.avoiding() == 0 {
		t.Fatal("the held drain was released")
	}
	f.s.holds.release(item.ID)
}

// A node whose agent reconnects under every drain: after maxDrainRequeues
// returns to the queue its next dispatch does not wait, so the node is
// upgraded instead of draining forever.
func TestANodeThatReconnectsUnderEveryDrainIsUpgradedAnyway(t *testing.T) {
	f := drainFixture(t)
	r := f.create(t, "restless", []string{"n1"}, 0)
	session := 0
	f.gw.inflight = func([]string) int {
		session++
		f.tr.set("n1", Connection{SessionID: fmt.Sprintf("s%d", session), Version: "old", Ready: true, Caps: []string{proto.CapUpgradePush}})
		return 0
	}
	for range maxDrainRequeues {
		f.tick(t)
		if f.tr.count() != 0 {
			t.Fatalf("dispatched during a drain whose node reconnected: %v", f.tr.callList())
		}
	}
	looks := f.gw.looksTaken()
	f.tick(t)
	if f.tr.count() != 1 {
		t.Fatalf("calls=%v; want the dispatch to stop waiting after %d requeues", f.tr.callList(), maxDrainRequeues)
	}
	if f.gw.looksTaken() != looks {
		t.Fatal("the dispatch after the cap waited again")
	}
	if i := f.item(t, r.Record.ID, "n1"); i.Phase != "waiting_registration" {
		t.Fatalf("item = %+v; want it dispatched", i)
	}
	// Once the attempt is no longer pending its count is forgotten.
	f.tr.set("n1", Connection{SessionID: "new-n1", Version: "target", Ready: true, Caps: []string{proto.CapUpgradePush}})
	f.tick(t)
	f.tick(t)
	if item, err := f.st.AgentUpgradeItem(r.Record.ID, "n1"); err != nil || f.s.holds.requeues(item.ID) != 0 {
		t.Fatalf("requeue count kept after the attempt settled: %v", err)
	}
}

// A store failure while the drain is planned leaves the attempt queued: the
// plan is read before the claim, so a failed read claims nothing, holds no
// slot and counts toward no breaker; the next pass dispatches it.
func TestADrainPlanThatCannotBeReadLeavesTheAttemptQueued(t *testing.T) {
	f := drainFixture(t)
	r := f.create(t, "locked", []string{"n1"}, 0)
	if err := f.st.DB().Exec("ALTER TABLE instances RENAME TO instances_away").Error; err != nil {
		t.Fatal(err)
	}
	if err := f.s.reconcile(context.Background()); err == nil {
		t.Fatal("reconcile hid the failed read")
	}
	if i := f.item(t, r.Record.ID, "n1"); i.Phase != "queued" || i.StartedAt != nil || i.BaselineSessionID != "" {
		t.Fatalf("item = %+v; want it still queued and unclaimed", i)
	}
	if f.gw.avoiding() != 0 || f.s.holds.count() != 0 {
		t.Fatal("a failed plan left a preference behind")
	}
	if err := f.st.DB().Exec("ALTER TABLE instances_away RENAME TO instances").Error; err != nil {
		t.Fatal(err)
	}
	f.gw.inflight = func([]string) int { return 0 }
	f.tick(t)
	if f.tr.count() != 1 {
		t.Fatalf("calls=%v; want the attempt dispatched on the next pass", f.tr.callList())
	}
}

// A failure to dispatch a drained attempt does not leave it holding the slot:
// the next pass returns it to the queue, unclaimed, and dispatches it.
func TestADrainedAttemptThatCannotBeDispatchedReturnsToTheQueue(t *testing.T) {
	f := drainFixture(t)
	r := f.create(t, "gone", []string{"n1"}, 0)
	// The node reconnects on the target version while it drains: there is
	// nothing to dispatch, and the attempt is judged again from the queue.
	f.gw.inflight = func([]string) int {
		f.tr.set("n1", Connection{SessionID: "other-n1", Version: "target", Ready: true, Caps: []string{proto.CapUpgradePush}})
		return 0
	}
	f.tick(t)
	if f.tr.count() != 0 {
		t.Fatal("dispatched to a node that reconnected on the target version")
	}
	if f.gw.avoiding() != 0 {
		t.Fatal("the requeued attempt kept its preference")
	}
	i := f.item(t, r.Record.ID, "n1")
	if i.Phase == "draining" || i.Phase == "transferring" {
		t.Fatalf("item = %+v; want it out of the slot", i)
	}
}

// The gateway's word decides which deployments wait: one it does not serve
// elsewhere (its other replica asleep, on an offline node, or absent) is not
// avoided and not waited for, so the upgrade dispatches at once.
func TestDeploymentNotServedElsewhereIsNotDrained(t *testing.T) {
	f := drainFixture(t)
	f.gw.setServed(f.deploymentID(7), false)
	r := f.create(t, "alone", []string{"n1"}, 0)
	f.gw.inflight = func([]string) int { return 5 }
	f.tick(t)
	if calls := f.gw.preferenceCalls(); len(calls) != 0 {
		t.Fatalf("a deployment not served elsewhere was avoided: %v", calls)
	}
	if looks := f.gw.looksTaken(); looks != 0 {
		t.Fatalf("a deployment not served elsewhere was waited for %d time(s)", looks)
	}
	if i := f.item(t, r.Record.ID, "n1"); i.Phase != "waiting_registration" || i.Reason != "" {
		t.Fatalf("item = %+v; want it dispatched at once", i)
	}
}

// The other replica stops serving mid-wait (it fell asleep, its node went
// offline): the deployment leaves the wait at once and its preference ends, so
// its requests keep their node until the restart instead of waiting it out.
func TestASiblingLostMidDrainEndsThatDeploymentsWait(t *testing.T) {
	f := drainFixture(t)
	r := f.create(t, "lost", []string{"n1"}, 0)
	f.s.cfg.DrainTimeout = time.Minute
	looks := 0
	f.gw.inflight = func([]string) int {
		looks++
		if looks == 2 {
			f.gw.setServed(f.deploymentID(7), false)
		}
		return 4 // never drains: only losing the sibling can end the wait early
	}
	start := time.Now()
	f.tick(t)
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("the wait ended after %v; want within a look of the sibling's loss", elapsed)
	}
	if f.tr.count() != 1 {
		t.Fatalf("calls=%v; want the upgrade dispatched once nothing was left to wait for", f.tr.callList())
	}
	if got := f.gw.preferenceCalls(); len(got) != 2 || got[1] != fmt.Sprintf("release:%d:inst-1", f.deploymentID(7)) {
		t.Fatalf("preference changes = %v; want it released when the sibling was lost", got)
	}
	if i := f.item(t, r.Record.ID, "n1"); i.Reason != "" {
		t.Fatalf("item reason = %q; nothing was cut by the bound", i.Reason)
	}
}

// A failed transfer most likely left the agent running: the node's preference
// ends at once rather than for as long as the outcome stays open.
func TestAFailedTransferEndsThePreference(t *testing.T) {
	f := drainFixture(t)
	r := f.create(t, "failing", []string{"n1"}, 0)
	f.gw.inflight = func([]string) int { return 0 }
	f.tr.pushErr = errors.New("read upgrade answer: EOF")
	f.tick(t)
	if i := f.item(t, r.Record.ID, "n1"); i.Phase != "observing_recovery" {
		t.Fatalf("item = %+v; want the uncertain transport outcome", i)
	}
	if f.gw.avoiding() != 0 {
		t.Fatal("a failed transfer kept the node avoided")
	}
}

// A runner cancelled mid-drain dispatches nothing and ends the preference; a
// restart returns the attempt to the queue, unclaimed, and dispatches it.
func TestADrainCancelledByShutdownIsRequeuedOnRestart(t *testing.T) {
	f := drainFixture(t)
	r := f.create(t, "cancelled", []string{"n1"}, 0)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	looks := 0
	f.gw.inflight = func([]string) int {
		if looks++; looks == 2 {
			cancel()
		}
		return 4
	}
	f.s.cfg.DrainTimeout = time.Minute
	if err := f.s.reconcile(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("reconcile = %v; want the cancellation", err)
	}
	if f.tr.count() != 0 || f.gw.avoiding() != 0 {
		t.Fatalf("calls=%v avoiding=%d; want nothing dispatched and nothing avoided", f.tr.callList(), f.gw.avoiding())
	}
	if i := f.item(t, r.Record.ID, "n1"); i.Phase != "draining" {
		t.Fatalf("item = %+v; want the stage it stopped in", i)
	}
	restarted, err := New(f.st, f.cfg)
	if err != nil {
		t.Fatal(err)
	}
	if i := f.item(t, r.Record.ID, "n1"); i.Phase != "queued" || i.StartedAt != nil {
		t.Fatalf("item after a restart = %+v; want it queued and unclaimed", i)
	}
	f.gw.inflight = func([]string) int { return 0 }
	if err := restarted.reconcile(context.Background()); err != nil {
		t.Fatal(err)
	}
	if f.tr.count() != 1 {
		t.Fatal("the restarted runner did not dispatch the returned attempt")
	}
}

// An operator's stop takes effect within one look, cancels the node that had
// not started, and ends its preference.
func TestAStopWhileDrainingCancelsTheNode(t *testing.T) {
	f := drainFixture(t)
	r := f.create(t, "stopped", []string{"n1"}, 0)
	looks := 0
	f.gw.inflight = func([]string) int {
		if looks++; looks == 2 {
			if _, err := f.s.Stop(context.Background(), r.Record.ID, "admin"); err != nil {
				t.Fatalf("stop: %v", err)
			}
		}
		return 1
	}
	f.s.cfg.DrainTimeout = time.Minute
	start := time.Now()
	f.tick(t)
	if elapsed := time.Since(start); elapsed > 10*time.Second {
		t.Fatalf("the stop took effect after %v; want within a look", elapsed)
	}
	if f.tr.count() != 0 || f.gw.avoiding() != 0 {
		t.Fatalf("calls=%v avoiding=%d; want nothing dispatched and nothing avoided", f.tr.callList(), f.gw.avoiding())
	}
	if i := f.item(t, r.Record.ID, "n1"); i.Phase != "cancelled" || i.Reason != "stopped" || i.StartedAt != nil {
		t.Fatalf("item = %+v; want it cancelled before it started", i)
	}
}

// A stop that this runner's drain did not see (a shutdown or a store
// failure ended the drain first): the stop itself cancelled the draining
// attempt, so neither the next pass nor a restart puts it back in the queue of
// a stopped batch, where it would never run and would block every new batch.
func TestAStopTheDrainMissedLeavesNothingQueued(t *testing.T) {
	f := newFixture(t, 3, true)
	r := f.create(t, "stop-unseen", []string{"n1"}, 0)
	item, err := f.st.AgentUpgradeItem(r.Record.ID, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := f.st.ClaimAgentUpgradeDrain(item.ID, f.now, f.s.heldSince()); err != nil || !claimed {
		t.Fatalf("claim the drain: %v, %v", claimed, err)
	}
	if _, err := f.s.Stop(context.Background(), r.Record.ID, "admin"); err != nil {
		t.Fatal(err)
	}
	f.tick(t)
	if _, err := New(f.st, f.cfg); err != nil {
		t.Fatal(err)
	}
	if i := f.item(t, r.Record.ID, "n1"); i.Phase != "cancelled" {
		t.Fatalf("item = %+v; want it cancelled with its batch", i)
	}
	if f.tr.count() != 0 {
		t.Fatal("a stopped batch dispatched its node")
	}
	// Nothing of the stopped batch holds the platform: a new batch starts.
	f.create(t, "after", []string{"n3"}, 0)
}

// An attempt found in the draining stage with no drain held here dispatched
// nothing: the next pass returns it to the queue, unclaimed, instead of
// letting it hold the platform's slot until the server restarts.
func TestAbandonedDrainingAttemptReturnsToTheQueue(t *testing.T) {
	f := newFixture(t, 1, true)
	r := f.create(t, "abandoned", nil, 0)
	item, err := f.st.AgentUpgradeItem(r.Record.ID, "n1")
	if err != nil {
		t.Fatal(err)
	}
	if claimed, err := f.st.ClaimAgentUpgradeDrain(item.ID, f.now, f.s.heldSince()); err != nil || !claimed {
		t.Fatalf("claim the drain: %v, %v", claimed, err)
	}
	f.now = f.now.Add(2 * time.Hour)
	f.tick(t)
	if f.tr.count() != 1 {
		t.Fatalf("calls=%v; want the returned attempt dispatched", f.tr.callList())
	}
}

func TestNodeServingNothingIsUpgradedAtOnce(t *testing.T) {
	f := newFixture(t, 1, true)
	r := f.create(t, "idle", nil, 0)
	f.tick(t)
	if f.tr.count() != 1 || len(f.gw.preferenceCalls()) != 0 || f.gw.looksTaken() != 0 {
		t.Fatalf("calls=%v preferences=%v looks=%d; want a dispatch at once", f.tr.callList(), f.gw.preferenceCalls(), f.gw.looksTaken())
	}
	if i := f.item(t, r.Record.ID, "n1"); i.Phase != "waiting_registration" || i.Error != "" {
		t.Fatalf("item = %+v", i)
	}
}
