package pushstate

import (
	"slices"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/fleetcap"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

// keepBody is capacityBody with a keep list, such as "3" or "2,3".
func keepBody(nodes, cards int, keep string) string {
	return strings.Replace(capacityBody(nodes, cards), `"refuse_card"`, `"keep":[`+keep+`],"refuse_card"`, 1)
}

func threeNodes(h *harness) {
	h.setFleet(fleetcap.Node{ID: 1, Cards: 8}, fleetcap.Node{ID: 2, Cards: 8}, fleetcap.Node{ID: 3, Cards: 8})
}

func TestAPushedKeepDecidesWhichNodesAreHeldAndDroppingItRestoresEnrollmentOrder(t *testing.T) {
	h := newHarness(t, openStore(t, ""), newFakeClock("2026-12-01T00:00:00Z")).load()
	threeNodes(h)
	authority := pusherOf(acmeAuthority)
	h.push2(authority, extapi.KindFleetCapacity, 1, capacityBody(2, 64))
	h.selectNow()
	if got := heldNodes(h, 1, 2, 3); !slices.Equal(got, []uint{3}) {
		t.Fatalf("held by enrollment order = %v", got)
	}

	before := h.reapply.Load()
	h.push2(authority, extapi.KindFleetCapacity, 2, keepBody(2, 64, "3"))
	h.selectNow()
	if got := heldNodes(h, 1, 2, 3); !slices.Equal(got, []uint{2}) {
		t.Fatalf("held with node 3 kept = %v", got)
	}
	if h.reapply.Load() == before {
		t.Fatal("a changed keep asked for no pass of the scheduling holds")
	}

	h.push2(authority, extapi.KindFleetCapacity, 3, capacityBody(2, 64))
	h.selectNow()
	if got := heldNodes(h, 1, 2, 3); !slices.Equal(got, []uint{3}) {
		t.Fatalf("held after the keep was dropped = %v", got)
	}
}

// The kept nodes of several owners are unioned while the caps take the
// tightest: one owner's keep counts under another owner's cap.
func TestKeepIsUnionedAcrossOwners(t *testing.T) {
	h := newHarness(t, openStore(t, ""), newFakeClock("2026-12-01T00:00:00Z")).load()
	h.setFleet(fleetcap.Node{ID: 1, Cards: 8}, fleetcap.Node{ID: 2, Cards: 8}, fleetcap.Node{ID: 3, Cards: 8}, fleetcap.Node{ID: 4, Cards: 8})
	h.push2(pusherOf(acmeAuthority), extapi.KindFleetCapacity, 1, keepBody(3, 64, "3"))
	h.push2(pusherOf(rogueAuthority), extapi.KindFleetCapacity, 1, keepBody(2, 64, "4"))
	h.selectNow()
	if got := heldNodes(h, 1, 2, 3, 4); !slices.Equal(got, []uint{1, 2}) {
		t.Fatalf("held = %v, want the unkept nodes under the tighter cap", got)
	}
	if c := h.s.cur.Load(); !slices.Equal(c.keep, []uint{3, 4}) {
		t.Fatalf("combined keep = %v", c.keep)
	}
}

// A node listed in keep before it exists is judged like any other newcomer
// until the selection places it, and is then kept first, holding an older
// node instead.
func TestANodeKeptBeforeItJoinsIsPlacedFirstOnlyOnceSelected(t *testing.T) {
	h := newHarness(t, openStore(t, ""), newFakeClock("2026-12-01T00:00:00Z")).load()
	h.setFleet(fleetcap.Node{ID: 1, Cards: 8}, fleetcap.Node{ID: 2, Cards: 8})
	h.push2(pusherOf(acmeAuthority), extapi.KindFleetCapacity, 1, keepBody(2, 64, "3"))
	h.selectNow()
	if heldNodes(h, 1, 2) != nil {
		t.Fatal("nodes within the caps are held")
	}
	if refusal := h.s.GuardNode(2, 16); refusal == nil {
		t.Fatal("an enrollment past a full node cap is not refused, kept or not")
	}
	// Node 3 joins anyway (it was counted once its cards were reported):
	// before the selection it is a newcomer past a full fleet.
	if !h.s.Hold(3, 8).held() {
		t.Fatal("a kept newcomer is not held before the selection places it")
	}
	threeNodes(h)
	h.selectNow()
	if got := heldNodes(h, 1, 2, 3); !slices.Equal(got, []uint{2}) {
		t.Fatalf("held once placed = %v", got)
	}
}

// The kept nodes survive a core restart with their table.
func TestKeepIsEnforcedAfterARestart(t *testing.T) {
	dsn := durableDSN(t)
	clk := newFakeClock("2026-12-01T00:00:00Z")
	{
		st := openStore(t, dsn)
		h := newHarness(t, st, clk).load().run()
		h.push(pusherOf(acmeAuthority), extapi.KindFleetCapacity, 1, keepBody(2, 64, "3"))
		h.stop()
		_ = st.Close()
	}
	h := newHarness(t, openStore(t, dsn), clk)
	threeNodes(h)
	h.load()
	clk.advance(time.Second)
	h.selectNow()
	if got := heldNodes(h, 1, 2, 3); !slices.Equal(got, []uint{2}) {
		t.Fatalf("held after restart = %v", got)
	}
}
