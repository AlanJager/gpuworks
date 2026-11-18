package pushstate

import (
	"context"
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/featuregate"
	"gpuworks.ai/gpuworks/internal/server/fleetcap"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

func TestAnOverrideReleasesOneHoldUntilTheOwnerDecidesAgain(t *testing.T) {
	st := openStore(t, "")
	h := newHarness(t, st, newFakeClock("2026-12-01T00:00:00Z")).load().run()
	ops := pusherOf(acmeOps, "push:node_holds")
	h.push(ops, extapi.KindNodeHolds, 1, opsHolds)
	h.s.OwnerDown("acme_ops")
	before := h.reapply.Load()

	o, err := h.s.Override(context.Background(), OverrideRequest{Owner: "acme_ops", Kind: extapi.KindNodeHolds, Entry: "12", Action: OverrideDelete, Reason: "maintenance done", ByUser: 1})
	if err != nil || o.ID == 0 || o.Generation != 1 {
		t.Fatalf("override = %+v, %v", o, err)
	}
	if h.s.Hold(12, 8).held() || !h.s.Hold(9, 8).held() {
		t.Fatal("the override released the wrong holds")
	}
	if h.reapply.Load() == before {
		t.Fatal("the override did not ask for a pass of the holds")
	}
	if list := h.s.Overrides(); len(list) != 1 || list[0].Reason != "maintenance done" {
		t.Fatalf("overrides = %+v", list)
	}

	// A push that leaves the entry as it was keeps the override.
	h.s.OwnerUp("acme_ops", 1)
	h.push(ops, extapi.KindNodeHolds, 2, `{"nodes":{"12":{"code":"ACME_MAINTENANCE","params":{"ticket":"OPS-118"}},"9":{"code":"ACME_QUARANTINE"},"5":{"code":"ACME_QUARANTINE"}}}`)
	if h.s.Hold(12, 8).held() || len(h.s.Overrides()) != 1 {
		t.Fatal("a push that did not touch the entry superseded the override")
	}
	// A push that changes it supersedes the override.
	h.push(ops, extapi.KindNodeHolds, 3, `{"nodes":{"12":{"code":"ACME_QUARANTINE"},"9":{"code":"ACME_QUARANTINE"}}}`)
	if !h.s.Hold(12, 8).held() || len(h.s.Overrides()) != 0 {
		t.Fatal("the owner's new decision did not replace the override")
	}
	h.mu.Lock()
	dropped := h.dropped
	h.mu.Unlock()
	if len(dropped) != 1 || dropped[0].Entry != "12" {
		t.Fatalf("superseded = %+v", dropped)
	}
	snap, _ := st.LoadPushedState(context.Background())
	if len(snap.Overrides) != 0 {
		t.Fatal("the superseded override is still stored")
	}
}

func TestAnOverrideRaisesACapAndSurvivesARestart(t *testing.T) {
	st := openStore(t, "")
	h := newHarness(t, st, newFakeClock("2026-12-01T00:00:00Z")).load().run()
	h.setFleet(fleetcap.Node{ID: 1, Cards: 8}, fleetcap.Node{ID: 2, Cards: 8})
	h.push(pusherOf(acmeAuthority), extapi.KindFleetCapacity, 1, capacityBody(1, 64))
	h.waitFor("node 2 held", func() bool { return h.s.Hold(2, 8).held() })
	if _, err := h.s.Override(context.Background(), OverrideRequest{Owner: "acme_authority", Kind: extapi.KindFleetCapacity, Entry: EntryMaxNodesWithCards, Action: OverrideLoosen, Value: i64(2), ByUser: 1}); err != nil {
		t.Fatal(err)
	}
	h.waitFor("node 2 released", func() bool { return !h.s.Hold(2, 8).held() })
	if h.s.GuardNode(1, 8) != nil || h.s.GuardNode(2, 8) == nil {
		t.Fatal("the raised cap is not what the guard enforces")
	}
	h.stop()
	h2 := newHarness(t, st, newFakeClock("2026-12-01T00:00:00Z")).load()
	if h2.s.GuardNode(1, 8) != nil {
		t.Fatal("the override did not survive a restart")
	}
}

func TestOverrideRefusals(t *testing.T) {
	h := newHarness(t, openStore(t, ""), newFakeClock("2026-12-01T00:00:00Z")).load().run()
	h.push(pusherOf(acmeOps, "push:node_holds"), extapi.KindNodeHolds, 1, opsHolds)
	h.push(pusherOf(acmeAuthority), extapi.KindFleetCapacity, 1, `{"hold":{"code":"ACME_OVER_CAPACITY"},"refuse_card":{"code":"ACME_CARD_LIMIT"},"refuse_node":{"code":"ACME_NODE_LIMIT"},"select":"enrollment_order","steps":[{"max_cards":16}]}`)
	h.push(pusherOf(acmeAuthority), extapi.KindAvailability, 1, backupFull)
	for _, tc := range []struct {
		name string
		req  OverrideRequest
		rule string
	}{
		{"a grant", OverrideRequest{Owner: "acme_authority", Kind: extapi.KindAvailability, Entry: "acme.backup", Action: OverrideDelete}, "not_restrict"},
		{"no such table", OverrideRequest{Owner: "acme_backup", Kind: extapi.KindNodeHolds, Entry: "12", Action: OverrideDelete}, "no_entry"},
		{"no such node", OverrideRequest{Owner: "acme_ops", Kind: extapi.KindNodeHolds, Entry: "13", Action: OverrideDelete}, "no_entry"},
		{"loosening a hold", OverrideRequest{Owner: "acme_ops", Kind: extapi.KindNodeHolds, Entry: "12", Action: OverrideLoosen, Value: i64(1)}, "action"},
		{"a cap that is not set", OverrideRequest{Owner: "acme_authority", Kind: extapi.KindFleetCapacity, Entry: EntryMaxNodesWithCards, Action: OverrideDelete}, "no_entry"},
		{"no such cap", OverrideRequest{Owner: "acme_authority", Kind: extapi.KindFleetCapacity, Entry: "max_gpus", Action: OverrideDelete}, "no_entry"},
		{"tightening a cap", OverrideRequest{Owner: "acme_authority", Kind: extapi.KindFleetCapacity, Entry: EntryMaxCards, Action: OverrideLoosen, Value: i64(8)}, "not_looser"},
		{"loosen without a value", OverrideRequest{Owner: "acme_authority", Kind: extapi.KindFleetCapacity, Entry: EntryMaxCards, Action: OverrideLoosen}, "action"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := h.s.Override(context.Background(), tc.req)
			var pe *messages.ProductError
			if !errors.As(err, &pe) || pe.Code() != messages.ErrorPluginOverrideRejected || pe.Params()["rule"] != tc.rule {
				t.Fatalf("override = %v; want rule %s", err, tc.rule)
			}
		})
	}
	if len(h.s.Overrides()) != 0 {
		t.Fatal("a refused override was stored")
	}
}

func TestAnOverrideWhoseCommitFailsIsNotApplied(t *testing.T) {
	st := openStore(t, "")
	h := newHarness(t, st, newFakeClock("2026-12-01T00:00:00Z")).load().run()
	h.push(pusherOf(acmeOps, "push:node_holds"), extapi.KindNodeHolds, 1, opsHolds)
	st.failWrites.Store(1)
	if _, err := h.s.Override(context.Background(), OverrideRequest{Owner: "acme_ops", Kind: extapi.KindNodeHolds, Entry: "12", Action: OverrideDelete}); !errors.Is(err, errInjected) {
		t.Fatalf("override = %v", err)
	}
	if !h.s.Hold(12, 8).held() || len(h.s.Overrides()) != 0 {
		t.Fatal("an override whose commit failed was applied")
	}
}

func TestUninstallReleasesEverythingTheOwnerPushed(t *testing.T) {
	st := openStore(t, "")
	h := newHarness(t, st, newFakeClock("2026-12-01T00:00:00Z")).load().run()
	authority := pusherOf(acmeAuthority, "push:banners", "push:node_holds")
	h.push(authority, extapi.KindAvailability, 1, backupFull)
	h.push(authority, extapi.KindFleetCapacity, 1, capacityBody(1, 8))
	h.push(authority, extapi.KindNodeHolds, 1, opsHolds)
	h.push(authority, extapi.KindBanners, 1, authorityBanners)
	h.push(pusherOf(acmeOps, "push:node_holds"), extapi.KindNodeHolds, 1, `{"nodes":{"7":{"code":"ACME_QUARANTINE"}}}`)
	h.s.OwnerDown("acme_authority")
	if _, err := h.s.Override(context.Background(), OverrideRequest{Owner: "acme_authority", Kind: extapi.KindNodeHolds, Entry: "12", Action: OverrideDelete}); err != nil {
		t.Fatal(err)
	}
	before := h.reapply.Load()

	h.s.SetPackages([]Package{acmeBackup, acmeOps})
	if err := h.s.Uninstall(context.Background(), "acme_authority"); err != nil {
		t.Fatal(err)
	}
	if h.s.GuardNode(5, 500) != nil || h.s.Hold(12, 8).held() || h.s.Hold(9, 8).held() || len(h.s.Banners(true)) != 0 {
		t.Fatal("uninstall left a restriction or a banner")
	}
	if !h.s.Hold(7, 8).held() {
		t.Fatal("uninstall released another owner's hold")
	}
	if a := h.s.Availability("acme.backup"); a.Access != featuregate.Locked || a.Notice.Code() != messages.ErrorFeatureProviderAbsent {
		t.Fatalf("a key of a namespace without its authority: %+v", a)
	}
	if h.reapply.Load() == before {
		t.Fatal("uninstall did not ask for a pass of the holds")
	}
	snap, err := st.LoadPushedState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, o := range snap.Owners {
		if o.Owner == "acme_authority" {
			t.Fatalf("uninstall left %+v", o)
		}
	}
	for _, d := range snap.Down {
		if d.Owner == "acme_authority" {
			t.Fatal("uninstall left the down record")
		}
	}
	if len(snap.Overrides) != 0 || len(snap.Rows.Banners) != 0 || len(snap.Rows.CapacitySteps) != 0 || len(snap.Rows.NodeHolds) != 1 {
		t.Fatalf("uninstall left rows: %+v", snap.Rows)
	}
	if len(h.s.Held("acme_authority")) != 0 {
		t.Fatal("uninstall left held state in memory")
	}
	// It can push nothing more.
	if _, w := h.try(authority, extapi.KindAvailability, 2, backupFull); w == nil || w.Code != messages.ErrorPluginStateNotOwner {
		t.Fatalf("push after uninstall = %+v", w)
	}
}
