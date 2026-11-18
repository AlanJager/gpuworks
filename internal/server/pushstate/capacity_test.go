package pushstate

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/fleetcap"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

func heldNodes(h *harness, ids ...uint) []uint {
	var out []uint
	for _, id := range ids {
		if h.s.Hold(id, 8).held() {
			out = append(out, id)
		}
	}
	return out
}

func TestCapacityKeepsNodesInEnrollmentOrderAndPlacesNewcomers(t *testing.T) {
	h := newHarness(t, openStore(t, ""), newFakeClock("2026-12-01T00:00:00Z")).load().run()
	h.setFleet(fleetcap.Node{ID: 1, Cards: 8}, fleetcap.Node{ID: 2, Cards: 8}, fleetcap.Node{ID: 3, Cards: 8}, fleetcap.Node{ID: 4, Cards: 0})
	h.push(pusherOf(acmeAuthority), extapi.KindFleetCapacity, 1, capacityBody(2, 24))
	h.waitFor("the selection to place the fleet", func() bool { return h.s.Hold(3, 8).held() })
	if got := heldNodes(h, 1, 2, 3); len(got) != 1 || got[0] != 3 {
		t.Fatalf("held = %v", got)
	}
	if hold := h.s.Hold(3, 8); hold.Code != "ACME_OVER_CAPACITY" {
		t.Fatalf("hold = %+v", hold)
	}
	if h.reapply.Load() == 0 {
		t.Fatal("the selection did not ask for a pass of the holds")
	}
	// A node the selection has not placed yet: held, because the fleet is
	// already full and a later node never displaces an earlier one.
	if !h.s.Hold(9, 1).held() {
		t.Fatal("a newcomer past a full fleet is not held")
	}
	// A cardless node is never held, and gaining cards is judged against
	// the room left.
	if h.s.Hold(4, 0).held() || !h.s.Hold(4, 2).held() {
		t.Fatal("cardless node")
	}
}

func TestANewcomerIsHeldOnlyIfCountingItExceedsACap(t *testing.T) {
	h := newHarness(t, openStore(t, ""), newFakeClock("2026-12-01T00:00:00Z")).load()
	h.setFleet(fleetcap.Node{ID: 1, Cards: 4}, fleetcap.Node{ID: 2, Cards: 4})
	h.push2(pusherOf(acmeAuthority), extapi.KindFleetCapacity, 1, capacityBody(3, 12))
	h.selectNow()
	if heldNodes(h, 1, 2) != nil {
		t.Fatal("nodes within the caps are held")
	}
	if h.s.Hold(3, 4).held() {
		t.Fatal("a newcomer that fits is held")
	}
	if !h.s.Hold(3, 5).held() {
		t.Fatal("a newcomer past the card cap is not held")
	}
	// A placed node that gained cards past the cap is held until placed.
	if h.s.Hold(2, 3).held() || h.s.Hold(2, 8).held() || !h.s.Hold(2, 9).held() {
		t.Fatal("a placed node whose cards changed")
	}
}

// push2 commits a push without a running tick: the test drives the tick.
func (h *harness) push2(p Pusher, kind string, gen uint64, body string) {
	h.t.Helper()
	done := make(chan *extapi.WireError, 1)
	go func() {
		_, w := h.try(p, kind, gen, body)
		done <- w
	}()
	h.waitFor("the push to queue", func() bool { h.s.mu.Lock(); defer h.s.mu.Unlock(); return len(h.s.pending) > 0 })
	h.s.commit(context.Background())
	if w := <-done; w != nil {
		h.t.Fatalf("push refused: %+v", w)
	}
}

// Nodes that join while the owner of the caps is down are placed by the
// core's rule, from the fleet topic, with no push.
func TestANodeJoiningWhileTheOwnerIsDownIsPlacedByTheCore(t *testing.T) {
	b := bus.New()
	h := newHarness(t, openStore(t, ""), newFakeClock("2026-12-01T00:00:00Z"), func(c *Config) { c.Bus, c.FleetTopic = b, "node_fleet" })
	h.setFleet(fleetcap.Node{ID: 1, Cards: 8})
	h.load().run()
	h.push(pusherOf(acmeAuthority), extapi.KindFleetCapacity, 1, capacityBody(2, 64))
	h.s.OwnerDown("acme_authority")
	h.waitFor("the first placement", func() bool { h.s.mu.Lock(); defer h.s.mu.Unlock(); return len(h.s.fleet) == 1 })

	h.setFleet(fleetcap.Node{ID: 1, Cards: 8}, fleetcap.Node{ID: 2, Cards: 8}, fleetcap.Node{ID: 3, Cards: 8})
	reads := h.reads.Load()
	for range 5 { // a burst of events: coalesced into few reads
		b.Publish(bus.Event{Topic: "node_fleet", Key: "3", Reason: "inserted"})
	}
	h.waitFor("node 3 to be placed and held", func() bool {
		h.s.mu.Lock()
		defer h.s.mu.Unlock()
		return h.s.heldByCap[3]
	})
	if heldNodes(h, 1, 2, 3)[0] != 3 || len(heldNodes(h, 1, 2, 3)) != 1 {
		t.Fatalf("held = %v", heldNodes(h, 1, 2, 3))
	}
	time.Sleep(20 * time.Millisecond)
	if n := h.reads.Load() - reads; n < 1 || n > 3 {
		t.Fatalf("five events took %d fleet reads", n)
	}
}

func TestTheSelectionDoesNotReadTheFleetWithoutACapAndRetriesAFailedRead(t *testing.T) {
	var fail bool
	h := newHarness(t, openStore(t, ""), newFakeClock("2026-12-01T00:00:00Z"))
	inner := h.s.cfg.Fleet
	h.s.cfg.Fleet = func(ctx context.Context) ([]fleetcap.Node, error) {
		if fail {
			return nil, errInjected
		}
		return inner(ctx)
	}
	h.load()
	h.s.fleetDirty.Store(true)
	if _, _, err := h.s.selectPass(); err != nil || h.reads.Load() != 0 {
		t.Fatalf("selection without caps read the fleet %d times (%v)", h.reads.Load(), err)
	}
	h.push2(pusherOf(acmeAuthority), extapi.KindFleetCapacity, 1, capacityBody(1, 8))
	fail = true
	if _, _, err := h.s.selectPass(); !errors.Is(err, errInjected) || !h.s.fleetDirty.Load() {
		t.Fatalf("failed read = %v, dirty %v", err, h.s.fleetDirty.Load())
	}
	fail = false
	h.setFleet(fleetcap.Node{ID: 1, Cards: 8}, fleetcap.Node{ID: 2, Cards: 8})
	held, _, err := h.s.selectPass()
	if err != nil || len(held) != 1 || held[0] != "2" {
		t.Fatalf("retry = %v, %v", held, err)
	}
}

func TestTheTightestCapWinsAcrossOwnersAndItsOwnerSpeaks(t *testing.T) {
	h := newHarness(t, openStore(t, ""), newFakeClock("2026-12-01T00:00:00Z")).load().run()
	h.push(pusherOf(acmeAuthority), extapi.KindFleetCapacity, 1, capacityBody(4, 8))
	h.push(pusherOf(rogueAuthority), extapi.KindFleetCapacity, 1, `{"hold":{"code":"ACME_OVER_CAPACITY"},"refuse_card":{"code":"ACME_CARD_LIMIT"},"refuse_node":{"code":"ACME_NODE_LIMIT","params":{"who":"rogue"}},"select":"enrollment_order","steps":[{"max_nodes_with_cards":2}]}`)
	if r := h.s.GuardNode(2, 0); r == nil || r.Params()["who"] != "rogue" {
		t.Fatalf("node cap refusal = %v", r)
	}
	if r := h.s.GuardNode(1, 8); r == nil || r.Code() != "ACME_CARD_LIMIT" || r.Params()["max"] != int64(8) {
		t.Fatalf("card cap refusal = %v", r)
	}
	if h.s.GuardNode(1, 7) != nil {
		t.Fatal("refused with room on both caps")
	}
}

func TestCapsFollowTheirTimeline(t *testing.T) {
	clk := newFakeClock("2027-01-31T00:00:00Z")
	h := newHarness(t, openStore(t, ""), clk).load()
	h.setFleet(fleetcap.Node{ID: 1, Cards: 8}, fleetcap.Node{ID: 2, Cards: 8})
	h.push2(pusherOf(acmeAuthority), extapi.KindFleetCapacity, 1, `{"hold":{"code":"ACME_OVER_CAPACITY"},"refuse_card":{"code":"ACME_CARD_LIMIT"},"refuse_node":{"code":"ACME_NODE_LIMIT"},"select":"enrollment_order","steps":[{"max_nodes_with_cards":8},{"at":"2027-02-01T00:00:00Z","max_nodes_with_cards":1}]}`)
	h.selectNow()
	if heldNodes(h, 1, 2) != nil {
		t.Fatal("held before the step")
	}
	clk.advance(24 * time.Hour)
	h.recombine()
	h.selectNow()
	if got := heldNodes(h, 1, 2); len(got) != 1 || got[0] != 2 {
		t.Fatalf("held after the step = %v", got)
	}
}

// When the caps are lifted (their owner removed), the selection asks for a
// pass of the scheduling holds even when no node it placed is released: a
// node held only because it was not placed yet must be released too.
func TestLiftingTheCapsReleasesNodesTheSelectionNeverPlaced(t *testing.T) {
	h := newHarness(t, openStore(t, ""), newFakeClock("2026-12-01T00:00:00Z")).load()
	h.setFleet(fleetcap.Node{ID: 1, Cards: 4})
	h.push2(pusherOf(acmeAuthority), extapi.KindFleetCapacity, 1, capacityBody(3, 4))
	h.selectNow()
	if !h.s.Hold(2, 4).held() {
		t.Fatal("a newcomer past the card cap is not held")
	}
	h.s.SetPackages(slices.DeleteFunc(slices.Clone(allPackages), func(p Package) bool { return p.ID == acmeAuthority.ID }))
	before := h.reapply.Load()
	h.selectNow()
	if h.s.Hold(2, 4).held() {
		t.Fatal("the newcomer is still held with no cap left")
	}
	if h.reapply.Load() == before {
		t.Fatal("lifting the caps asked for no pass of the scheduling holds")
	}
}
