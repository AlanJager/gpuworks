package pushstate

import (
	"context"
	"errors"
	"strconv"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

func overrideHold12(t *testing.T, h *harness, reason string) Override {
	t.Helper()
	o, err := h.s.Override(context.Background(), OverrideRequest{Owner: "acme_ops", Kind: extapi.KindNodeHolds, Entry: "12", Action: OverrideDelete, Reason: reason, ByUser: 1})
	if err != nil {
		t.Fatal(err)
	}
	return o
}

// Removing an override enforces the owner's entry again, and a restart
// finds it removed.
func TestRemovingAnOverrideReappliesTheHoldAndSurvivesARestart(t *testing.T) {
	st := openStore(t, "")
	h := newHarness(t, st, newFakeClock("2026-12-01T00:00:00Z")).load().run()
	h.push(pusherOf(acmeOps, "push:node_holds"), extapi.KindNodeHolds, 1, opsHolds)
	h.s.OwnerDown("acme_ops")
	o := overrideHold12(t, h, "maintenance done")
	if h.s.Hold(12, 8).held() {
		t.Fatal("the override did not release the hold")
	}
	before := h.reapply.Load()

	removed, err := h.s.RemoveOverride(context.Background(), o.ID)
	if err != nil || removed.ID != o.ID || removed.Reason != "maintenance done" {
		t.Fatalf("remove = %+v, %v", removed, err)
	}
	if !h.s.Hold(12, 8).held() || len(h.s.Overrides()) != 0 {
		t.Fatal("the hold is not enforced again after the removal")
	}
	if h.reapply.Load() == before {
		t.Fatal("the removal did not ask for a pass of the holds")
	}
	if _, err := h.s.RemoveOverride(context.Background(), o.ID); !isCode(err, messages.ErrorPluginOverrideNotFound) {
		t.Fatalf("a second removal = %v; want PLUGIN_OVERRIDE_NOT_FOUND", err)
	}
	h.stop()
	h2 := newHarness(t, st, newFakeClock("2026-12-01T00:00:00Z")).load()
	if !h2.s.Hold(12, 8).held() || len(h2.s.Overrides()) != 0 {
		t.Fatal("the removed override came back after a restart")
	}
}

// A removal whose commit fails leaves the override in force.
func TestARemovalWhoseCommitFailsKeepsTheOverride(t *testing.T) {
	st := openStore(t, "")
	h := newHarness(t, st, newFakeClock("2026-12-01T00:00:00Z")).load().run()
	h.push(pusherOf(acmeOps, "push:node_holds"), extapi.KindNodeHolds, 1, opsHolds)
	o := overrideHold12(t, h, "relief")
	st.failWrites.Store(1)
	if _, err := h.s.RemoveOverride(context.Background(), o.ID); !errors.Is(err, errInjected) {
		t.Fatalf("a removal whose commit failed = %v", err)
	}
	if h.s.Hold(12, 8).held() || len(h.s.Overrides()) != 1 {
		t.Fatal("a removal whose commit failed was applied")
	}
}

// A second override of the same entry replaces the first, in memory and
// in the store.
func TestASecondOverrideOfAnEntryReplacesTheFirst(t *testing.T) {
	st := openStore(t, "")
	h := newHarness(t, st, newFakeClock("2026-12-01T00:00:00Z")).load().run()
	h.push(pusherOf(acmeOps, "push:node_holds"), extapi.KindNodeHolds, 1, opsHolds)
	first := overrideHold12(t, h, "first")
	second := overrideHold12(t, h, "second")
	if list := h.s.Overrides(); len(list) != 1 || list[0].ID != second.ID || list[0].ID == first.ID {
		t.Fatalf("overrides = %+v", list)
	}
	snap, err := st.LoadPushedState(context.Background())
	if err != nil || len(snap.Overrides) != 1 || snap.Overrides[0].Reason != "second" {
		t.Fatalf("stored overrides = %+v, %v", snap.Overrides, err)
	}
}

// A supersede raises an administrator banner for a day; uninstalling the
// owner drops it with the rest of what the owner left.
func TestASupersededOverrideRaisesAnAdminBannerForADay(t *testing.T) {
	clk := newFakeClock("2026-12-01T00:00:00Z")
	h := newHarness(t, openStore(t, ""), clk).load().run()
	ops := pusherOf(acmeOps, "push:node_holds")
	h.push(ops, extapi.KindNodeHolds, 1, opsHolds)
	o := overrideHold12(t, h, "relief")
	h.push(ops, extapi.KindNodeHolds, 2, `{"nodes":{"12":{"code":"ACME_QUARANTINE"}}}`)

	want := "plugins.override_superseded." + strconv.FormatInt(o.ID, 10)
	b := bannerByID(h.s.Banners(true), want)
	if b == nil || b.Severity != extapi.SeverityWarning || b.Notice.Code() != messages.ErrorPluginOverrideSuperseded {
		t.Fatalf("admin banners = %+v; want %s", h.s.Banners(true), want)
	}
	if p := b.Notice.Params(); p["plugin"] != "acme_ops" || p["kind"] != extapi.KindNodeHolds || p["entry"] != "12" {
		t.Fatalf("banner params = %v", p)
	}
	if bannerByID(h.s.Banners(false), want) != nil {
		t.Fatal("the supersede banner is shown to everyone")
	}
	clk.advance(23 * time.Hour)
	h.recombine()
	if bannerByID(h.s.Banners(true), want) == nil {
		t.Fatal("the supersede banner left before a day")
	}
	clk.advance(time.Hour)
	h.recombine()
	if bannerByID(h.s.Banners(true), want) != nil {
		t.Fatal("the supersede banner outlived a day")
	}

	// Another supersede, then the owner's uninstall.
	o2 := overrideHold12(t, h, "relief again")
	h.push(ops, extapi.KindNodeHolds, 3, opsHolds)
	if bannerByID(h.s.Banners(true), "plugins.override_superseded."+strconv.FormatInt(o2.ID, 10)) == nil {
		t.Fatal("the second supersede raised no banner")
	}
	h.s.SetPackages([]Package{acmeAuthority, acmeBackup})
	if err := h.s.Uninstall(context.Background(), "acme_ops"); err != nil {
		t.Fatal(err)
	}
	if len(h.s.Banners(true)) != 0 {
		t.Fatalf("uninstall left banners: %+v", h.s.Banners(true))
	}
	h.s.mu.Lock()
	left := len(h.s.superseded)
	h.s.mu.Unlock()
	if left != 0 {
		t.Fatal("uninstall left a supersede in memory")
	}
}

func bannerByID(list []Banner, id string) *Banner {
	for i := range list {
		if list[i].ID == id {
			return &list[i]
		}
	}
	return nil
}

func isCode(err error, code string) bool {
	var pe *messages.ProductError
	return errors.As(err, &pe) && pe.Code() == code
}
