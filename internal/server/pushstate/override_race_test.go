package pushstate

import (
	"context"
	"errors"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// The tests here drive the writer tick by hand (no run), so a push and an
// administrator's change can be placed in one tick, or a push between the
// check of a change and the tick that commits it.

const opsHolds12Changed = `{"nodes":{"12":{"code":"ACME_MAINTENANCE","params":{"ticket":"OPS-131"}},"9":{"code":"ACME_QUARANTINE"}}}`

// queued waits until ops administrator changes and pushes pushes are queued.
func (h *harness) queued(ops, pushes int) {
	h.t.Helper()
	h.waitFor("the queued changes", func() bool {
		h.s.mu.Lock()
		defer h.s.mu.Unlock()
		return len(h.s.ops) == ops && len(h.s.pending) == pushes
	})
}

// seedOverride commits opsHolds and an override of node 12 by hand.
func seedOverride(t *testing.T, h *harness) Override {
	t.Helper()
	pushed := make(chan *extapi.WireError, 1)
	go func() {
		_, w := h.try(pusherOf(acmeOps, "push:node_holds"), extapi.KindNodeHolds, 1, opsHolds)
		pushed <- w
	}()
	h.queued(0, 1)
	h.s.commit(context.Background())
	if w := <-pushed; w != nil {
		t.Fatalf("push = %+v", w)
	}
	got := make(chan error, 1)
	var o Override
	go func() {
		var err error
		o, err = h.s.Override(context.Background(), OverrideRequest{Owner: "acme_ops", Kind: extapi.KindNodeHolds, Entry: "12", Action: OverrideDelete, Reason: "r", ByUser: 1})
		got <- err
	}()
	h.queued(1, 0)
	h.s.commit(context.Background())
	if err := <-got; err != nil {
		t.Fatal(err)
	}
	return o
}

// An override and a push that changes its entry, in one tick: the owner's
// new decision wins and the override is refused, not stored against the
// decision the push replaced.
func TestAnOverrideAndAChangingPushInOneTickRefuseTheOverride(t *testing.T) {
	h := newHarness(t, openStore(t, ""), newFakeClock("2026-12-01T00:00:00Z")).load()
	old := seedOverride(t, h)

	got := make(chan error, 1)
	go func() {
		_, err := h.s.Override(context.Background(), OverrideRequest{Owner: "acme_ops", Kind: extapi.KindNodeHolds, Entry: "12", Action: OverrideDelete, Reason: "again", ByUser: 1})
		got <- err
	}()
	h.queued(1, 0)
	pushed := make(chan *extapi.WireError, 1)
	go func() {
		_, w := h.try(pusherOf(acmeOps, "push:node_holds"), extapi.KindNodeHolds, 2, opsHolds12Changed)
		pushed <- w
	}()
	h.queued(1, 1)
	h.s.commit(context.Background())

	if err := <-got; !isCode(err, messages.ErrorPluginOverrideRejected) || paramOf(err, "rule") != "superseded" {
		t.Fatalf("override = %v; want PLUGIN_OVERRIDE_REJECTED rule superseded", err)
	}
	if w := <-pushed; w != nil {
		t.Fatalf("push = %+v", w)
	}
	if len(h.s.Overrides()) != 0 || !h.s.Hold(12, 8).held() {
		t.Fatalf("an override survived the push that changed its entry: %+v", h.s.Overrides())
	}
	h.s.mu.Lock()
	_, banner := h.s.superseded[old.ID]
	h.s.mu.Unlock()
	if !banner {
		t.Fatal("the override in force before the tick was not reported superseded")
	}
	snap, err := h.st.LoadPushedState(context.Background())
	if err != nil || len(snap.Overrides) != 0 {
		t.Fatalf("stored overrides = %+v, %v", snap.Overrides, err)
	}
}

// A push that changes the entry and commits between the check of an
// override and the tick that would store it refuses the override.
func TestAPushCommittedAfterTheCheckRefusesTheOverride(t *testing.T) {
	h := newHarness(t, openStore(t, ""), newFakeClock("2026-12-01T00:00:00Z")).load()
	seedOverride(t, h)

	got := make(chan error, 1)
	go func() {
		_, err := h.s.Override(context.Background(), OverrideRequest{Owner: "acme_ops", Kind: extapi.KindNodeHolds, Entry: "9", Action: OverrideDelete, Reason: "r", ByUser: 1})
		got <- err
	}()
	h.queued(1, 0)
	// Hold the checked override back while the push commits alone.
	h.s.mu.Lock()
	held := h.s.ops
	h.s.ops = nil
	h.s.mu.Unlock()
	pushed := make(chan *extapi.WireError, 1)
	go func() {
		_, w := h.try(pusherOf(acmeOps, "push:node_holds"), extapi.KindNodeHolds, 2, `{"nodes":{"12":{"code":"ACME_MAINTENANCE","params":{"ticket":"OPS-118"}},"9":{"code":"ACME_MAINTENANCE"}}}`)
		pushed <- w
	}()
	h.queued(0, 1)
	h.s.commit(context.Background())
	if w := <-pushed; w != nil {
		t.Fatalf("push = %+v", w)
	}
	h.s.mu.Lock()
	h.s.ops = held
	h.s.mu.Unlock()
	h.s.commit(context.Background())

	if err := <-got; !isCode(err, messages.ErrorPluginOverrideRejected) || paramOf(err, "rule") != "superseded" {
		t.Fatalf("override = %v; want PLUGIN_OVERRIDE_REJECTED rule superseded", err)
	}
	if !h.s.Hold(9, 8).held() {
		t.Fatal("an override of the owner's old decision released the hold")
	}
}

// A push that leaves the entry alone between the check and the commit does
// not refuse the override, and the override binds to the generation held
// when it is stored.
func TestAPushThatKeepsTheEntryBindsTheOverrideToItsGeneration(t *testing.T) {
	h := newHarness(t, openStore(t, ""), newFakeClock("2026-12-01T00:00:00Z")).load()
	seedOverride(t, h)

	got := make(chan error, 1)
	var o Override
	go func() {
		var err error
		o, err = h.s.Override(context.Background(), OverrideRequest{Owner: "acme_ops", Kind: extapi.KindNodeHolds, Entry: "9", Action: OverrideDelete, Reason: "r", ByUser: 1})
		got <- err
	}()
	h.queued(1, 0)
	h.s.mu.Lock()
	held := h.s.ops
	h.s.ops = nil
	h.s.mu.Unlock()
	pushed := make(chan *extapi.WireError, 1)
	go func() {
		_, w := h.try(pusherOf(acmeOps, "push:node_holds"), extapi.KindNodeHolds, 2, opsHolds12Changed)
		pushed <- w
	}()
	h.queued(0, 1)
	h.s.commit(context.Background())
	<-pushed
	h.s.mu.Lock()
	h.s.ops = held
	h.s.mu.Unlock()
	h.s.commit(context.Background())

	if err := <-got; err != nil {
		t.Fatal(err)
	}
	if o.Generation != 2 || h.s.Hold(9, 8).held() {
		t.Fatalf("override = %+v; want generation 2 releasing node 9", o)
	}
}

// A removal and a push that supersedes the same override, in one tick: the
// removal claims it, so it is not also reported superseded. A removal of an
// override a push superseded in an earlier tick is not found.
func TestARemovalAndASupersedingPushInOneTick(t *testing.T) {
	h := newHarness(t, openStore(t, ""), newFakeClock("2026-12-01T00:00:00Z")).load()
	o := seedOverride(t, h)

	got := make(chan error, 1)
	go func() { _, err := h.s.RemoveOverride(context.Background(), o.ID); got <- err }()
	h.queued(1, 0)
	pushed := make(chan *extapi.WireError, 1)
	go func() {
		_, w := h.try(pusherOf(acmeOps, "push:node_holds"), extapi.KindNodeHolds, 2, opsHolds12Changed)
		pushed <- w
	}()
	h.queued(1, 1)
	h.s.commit(context.Background())
	if err := <-got; err != nil {
		t.Fatalf("removal = %v", err)
	}
	<-pushed
	h.s.mu.Lock()
	_, banner := h.s.superseded[o.ID]
	h.s.mu.Unlock()
	if banner {
		t.Fatal("a removed override was also reported superseded")
	}

	o2 := func() Override {
		got := make(chan error, 1)
		var o Override
		go func() {
			var err error
			o, err = h.s.Override(context.Background(), OverrideRequest{Owner: "acme_ops", Kind: extapi.KindNodeHolds, Entry: "12", Action: OverrideDelete, Reason: "r", ByUser: 1})
			got <- err
		}()
		h.queued(1, 0)
		h.s.commit(context.Background())
		if err := <-got; err != nil {
			t.Fatal(err)
		}
		return o
	}()
	// The removal is checked, then a push supersedes the override first.
	go func() { _, err := h.s.RemoveOverride(context.Background(), o2.ID); got <- err }()
	h.queued(1, 0)
	h.s.mu.Lock()
	held := h.s.ops
	h.s.ops = nil
	h.s.mu.Unlock()
	go func() {
		_, w := h.try(pusherOf(acmeOps, "push:node_holds"), extapi.KindNodeHolds, 3, opsHolds)
		pushed <- w
	}()
	h.queued(0, 1)
	h.s.commit(context.Background())
	<-pushed
	h.s.mu.Lock()
	h.s.ops = held
	h.s.mu.Unlock()
	h.s.commit(context.Background())
	if err := <-got; !isCode(err, messages.ErrorPluginOverrideNotFound) {
		t.Fatalf("removal of a superseded override = %v; want PLUGIN_OVERRIDE_NOT_FOUND", err)
	}
}

// A caller that gives up after a tick took its change still gets the
// tick's answer, so a change that commits is never reported as refused;
// one that gives up before is withdrawn.
func TestAGivenUpChangeIsWithdrawnOrAnsweredByItsTick(t *testing.T) {
	h := newHarness(t, openStore(t, ""), newFakeClock("2026-12-01T00:00:00Z")).load()
	o := seedOverride(t, h)

	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := h.s.RemoveOverride(ctx, o.ID); err != context.Canceled {
		t.Fatalf("removal with a done context = %v", err)
	}
	if len(h.s.Overrides()) != 1 {
		t.Fatal("a withdrawn removal was applied")
	}

	ctx, cancel = context.WithCancel(context.Background())
	got := make(chan error, 1)
	go func() { _, err := h.s.RemoveOverride(ctx, o.ID); got <- err }()
	h.queued(1, 0)
	// Take the op as a tick does, then give up before it commits.
	h.s.mu.Lock()
	b, w := h.s.buildLocked()
	h.s.mu.Unlock()
	cancel()
	select {
	case err := <-got:
		t.Fatalf("enqueue returned %v before its tick answered", err)
	case <-time.After(50 * time.Millisecond):
	}
	res, err := h.st.WritePushedState(context.Background(), b)
	h.s.mu.Lock()
	h.s.applyLocked(b, w, res, err)
	h.s.mu.Unlock()
	for _, op := range w.ops {
		op.done <- err
	}
	if err := <-got; err != nil {
		t.Fatalf("removal = %v; want the commit's answer", err)
	}
	if len(h.s.Overrides()) != 0 {
		t.Fatal("the committed removal is not applied")
	}
}

func paramOf(err error, key string) any {
	var pe *messages.ProductError
	if !errors.As(err, &pe) {
		return nil
	}
	return pe.Params()[key]
}
