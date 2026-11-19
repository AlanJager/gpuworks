package pushstate

import (
	"context"
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

const opsBanner = `{"banners":[{"audience":"admin","id":"window","steps":[{"notice":{"code":"ACME_MAINTENANCE"},"severity":"info"}]}]}`

// Clearing a restriction table releases what it held and drops its
// overrides in one commit, leaves the owner's other kinds and every other
// owner alone, survives a restart, is idempotent, and lets the owner push
// the kind again from any generation.
func TestClearTableReleasesOnlyThatOwnersKindAndItsOverrides(t *testing.T) {
	st := openStore(t, "")
	h := newHarness(t, st, newFakeClock("2026-12-01T00:00:00Z")).load().run()
	ops := pusherOf(acmeOps, "push:node_holds", "push:banners")
	h.push(ops, extapi.KindNodeHolds, 4, opsHolds)
	h.push(ops, extapi.KindBanners, 1, opsBanner)
	h.push(pusherOf(acmeAuthority), extapi.KindFleetCapacity, 1, capacityBody(2, 24))
	h.s.OwnerDown("acme_ops")
	overrideHold12(t, h, "relief")
	if _, err := h.s.Override(context.Background(), OverrideRequest{Owner: "acme_authority", Kind: extapi.KindFleetCapacity,
		Entry: EntryMaxCards, Action: OverrideLoosen, Value: i64(48), Reason: "burst", ByUser: 1}); err != nil {
		t.Fatal(err)
	}
	if !h.s.Hold(9, 8).held() {
		t.Fatal("the pushed hold of node 9 is not enforced")
	}
	before := h.reapply.Load()

	for range 2 {
		if err := h.s.ClearTable(context.Background(), "acme_ops", extapi.KindNodeHolds); err != nil {
			t.Fatal(err)
		}
	}
	check := func(when string, s *State) {
		t.Helper()
		if s.Hold(9, 8).held() || s.Hold(12, 8).held() {
			t.Fatalf("%s: a cleared hold is still enforced", when)
		}
		list := s.Overrides()
		if len(list) != 1 || list[0].Owner != "acme_authority" || list[0].Kind != extapi.KindFleetCapacity {
			t.Fatalf("%s: overrides = %+v", when, list)
		}
		var kinds []string
		for _, held := range s.Held("acme_ops") {
			kinds = append(kinds, held.Kind)
		}
		if len(kinds) != 1 || kinds[0] != extapi.KindBanners {
			t.Fatalf("%s: acme_ops holds %v", when, kinds)
		}
		if held := s.Held("acme_authority"); len(held) != 1 || held[0].Kind != extapi.KindFleetCapacity {
			t.Fatalf("%s: acme_authority holds %+v", when, held)
		}
	}
	check("after the clear", h.s)
	if h.reapply.Load() == before {
		t.Fatal("the clear did not ask for a pass of the holds")
	}
	// The owner's next push starts a fresh table: a generation below the
	// cleared one is accepted.
	if _, w := h.try(ops, extapi.KindNodeHolds, 1, `{"nodes":{"9":{"code":"ACME_QUARANTINE"}}}`); w != nil {
		t.Fatalf("a push after the clear = %+v", w)
	}
	if !h.s.Hold(9, 8).held() || h.s.Hold(12, 8).held() {
		t.Fatal("the push after the clear is not what is enforced")
	}
	if err := h.s.ClearTable(context.Background(), "acme_ops", extapi.KindNodeHolds); err != nil {
		t.Fatal(err)
	}
	h.stop()

	h2 := newHarness(t, st, newFakeClock("2026-12-01T00:00:00Z")).load()
	check("after a restart", h2.s)
}

// Only restrictions are cleared: a grant, a banner or an attestation is
// removed only by its owner's push or its uninstall.
func TestClearTableRefusesAKindThatIsNotARestriction(t *testing.T) {
	h := newHarness(t, openStore(t, ""), newFakeClock("2026-12-01T00:00:00Z")).load().run()
	h.push(pusherOf(acmeAuthority), extapi.KindAvailability, 1, `{"rows":[{"down_access":"read_only","feature":"acme.backup","steps":[{"access":"full"}]}]}`)
	for _, kind := range []string{extapi.KindAvailability, extapi.KindBanners, extapi.KindAttestations, "unknown_kind"} {
		err := h.s.ClearTable(context.Background(), "acme_authority", kind)
		var pe *messages.ProductError
		if !errors.As(err, &pe) || pe.Code() != messages.ErrorBadRequest || pe.Params()["kind"] != kind || pe.Params()["plugin"] != "acme_authority" {
			t.Fatalf("clear %s = %v", kind, err)
		}
	}
	if held := h.s.Held("acme_authority"); len(held) != 1 {
		t.Fatalf("a refused clear changed what is held: %+v", held)
	}
}

// A clear whose commit fails changes nothing.
func TestAClearWhoseCommitFailsKeepsTheTable(t *testing.T) {
	st := openStore(t, "")
	h := newHarness(t, st, newFakeClock("2026-12-01T00:00:00Z")).load().run()
	h.push(pusherOf(acmeOps, "push:node_holds"), extapi.KindNodeHolds, 1, opsHolds)
	overrideHold12(t, h, "relief")
	st.failWrites.Store(1)
	if err := h.s.ClearTable(context.Background(), "acme_ops", extapi.KindNodeHolds); !errors.Is(err, errInjected) {
		t.Fatalf("a clear whose commit failed = %v", err)
	}
	if !h.s.Hold(9, 8).held() || len(h.s.Overrides()) != 1 || len(h.s.Held("acme_ops")) != 1 {
		t.Fatal("a clear whose commit failed was applied")
	}
}
