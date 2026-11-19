package pushstate

import (
	"context"
	"slices"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/pkg/extapi"
)

// The impact of uninstalling an owner lists exactly its rows: the holds
// only it places, the caps it sets, the keys it answers for, its banners
// and its overrides; another owner's hold of the same node is not
// released. Computing it changes nothing that is enforced.
func TestUninstallImpactListsExactlyTheOwnersRows(t *testing.T) {
	h := newHarness(t, openStore(t, ""), newFakeClock("2026-12-01T00:00:00Z")).load().run()
	authority := pusherOf(acmeAuthority, "push:banners", "push:node_holds")
	h.push(authority, extapi.KindAvailability, 1, backupFull)
	h.push(authority, extapi.KindFleetCapacity, 1, capacityBody(1, 8))
	h.push(authority, extapi.KindNodeHolds, 1, opsHolds) // nodes 9 and 12
	h.push(authority, extapi.KindBanners, 1, authorityBanners)
	h.push(pusherOf(acmeOps, "push:node_holds"), extapi.KindNodeHolds, 1, `{"nodes":{"9":{"code":"ACME_QUARANTINE"},"7":{"code":"ACME_QUARANTINE"}}}`)
	o, err := h.s.Override(context.Background(), OverrideRequest{Owner: "acme_authority", Kind: extapi.KindFleetCapacity, Entry: EntryMaxCards,
		Action: OverrideLoosen, Value: i64(16), Reason: "more cards", ByUser: 1})
	if err != nil {
		t.Fatal(err)
	}

	im, err := h.s.UninstallImpact("acme_authority", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	kinds := make([]string, len(im.Tables))
	for i, tb := range im.Tables {
		kinds[i] = tb.Kind
	}
	if !slices.Equal(kinds, []string{extapi.KindAvailability, extapi.KindBanners, extapi.KindFleetCapacity, extapi.KindNodeHolds}) {
		t.Fatalf("tables = %+v", im.Tables)
	}
	if !slices.Equal(im.Nodes, []uint{12}) {
		t.Fatalf("released nodes = %v; want 12 alone (acme_ops still holds 9)", im.Nodes)
	}
	if !slices.Equal(im.Caps, []string{EntryMaxNodesWithCards, EntryMaxCards}) {
		t.Fatalf("caps = %v", im.Caps)
	}
	// acme.reports is locked already: the authority pushed no row for it.
	if !slices.Equal(im.Features, []string{"acme.backup"}) {
		t.Fatalf("features locked = %v", im.Features)
	}
	if im.Banners != 2 || !slices.Equal(im.Overrides, []int64{o.ID}) || im.UsersAffected != 0 || !im.Digest.Valid() {
		t.Fatalf("impact = %+v", im)
	}
	if !h.s.Hold(12, 8).held() || h.s.GuardNode(5, 500) == nil || len(h.s.Banners(true)) != 2 {
		t.Fatal("computing the impact changed what is enforced")
	}

	other, err := h.s.UninstallImpact("acme_ops", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(other.Nodes, []uint{7}) || len(other.Caps)+len(other.Features)+other.Banners+len(other.Overrides) != 0 || len(other.Tables) != 1 {
		t.Fatalf("acme_ops impact = %+v", other)
	}
}

// The keys a package declares with gate none are full while it is
// installed and lock when it goes, though it pushed nothing.
func TestUninstallImpactLocksTheKeysOnlyThePackageDeclares(t *testing.T) {
	h := newHarness(t, openStore(t, ""), newFakeClock("2026-12-01T00:00:00Z")).load()
	im, _ := h.s.UninstallImpact("acme_backup", "1.0.0")
	if !slices.Equal(im.Features, []string{"acme.free"}) || len(im.Tables) != 0 {
		t.Fatalf("impact = %+v", im)
	}
}

// An owner holding nothing has a digest of its id and version alone, the
// same every time and at every instant; another version digests
// differently.
func TestUninstallImpactOfAnEmptyOwnerIsStable(t *testing.T) {
	clk := newFakeClock("2026-12-01T00:00:00Z")
	h := newHarness(t, openStore(t, ""), clk).load()
	a, err := h.s.UninstallImpact("acme_ops", "1.0.0")
	if err != nil {
		t.Fatal(err)
	}
	clk.advance(time.Hour)
	b, _ := h.s.UninstallImpact("acme_ops", "1.0.0")
	c, _ := h.s.UninstallImpact("acme_ops", "1.0.1")
	if a.Digest != b.Digest || a.Digest == c.Digest || len(a.Tables)+len(a.Nodes)+len(a.Features)+len(a.Caps)+a.Banners+len(a.Overrides) != 0 {
		t.Fatalf("empty impacts = %+v / %+v / %+v", a, b, c)
	}
}

// A push between the preview and the confirmation changes the digest, even
// one that changes no count.
func TestAPushChangesTheImpactDigest(t *testing.T) {
	h := newHarness(t, openStore(t, ""), newFakeClock("2026-12-01T00:00:00Z")).load().run()
	ops := pusherOf(acmeOps, "push:node_holds")
	h.push(ops, extapi.KindNodeHolds, 1, opsHolds)
	before, _ := h.s.UninstallImpact("acme_ops", "1.0.0")
	h.push(ops, extapi.KindNodeHolds, 2, opsHolds12Changed)
	after, _ := h.s.UninstallImpact("acme_ops", "1.0.0")
	if before.Digest == after.Digest || !slices.Equal(before.Nodes, after.Nodes) {
		t.Fatalf("digest before %s, after %s", before.Digest, after.Digest)
	}
}

// An uninstalled owner whose rows are still held (an uninstall that
// failed after the package went) still lists its tables; nothing it held is
// enforced, so nothing else changes, and the retried uninstall removes them.
func TestUninstallImpactOfAHalfUninstalledOwner(t *testing.T) {
	st := openStore(t, "")
	h := newHarness(t, st, newFakeClock("2026-12-01T00:00:00Z")).load().run()
	h.push(pusherOf(acmeOps, "push:node_holds"), extapi.KindNodeHolds, 1, opsHolds)
	h.s.SetPackages([]Package{acmeAuthority, acmeBackup})
	st.failWrites.Store(1)
	if err := h.s.Uninstall(context.Background(), "acme_ops"); err == nil {
		t.Fatal("the injected failure did not fail the uninstall")
	}
	im, _ := h.s.UninstallImpact("acme_ops", "")
	if len(im.Tables) != 1 || im.Tables[0].Kind != extapi.KindNodeHolds || im.Tables[0].Entries != 2 || len(im.Nodes) != 0 {
		t.Fatalf("impact of the half uninstalled owner = %+v", im)
	}
	if err := h.s.Uninstall(context.Background(), "acme_ops"); err != nil {
		t.Fatal(err)
	}
	if im, _ := h.s.UninstallImpact("acme_ops", ""); len(im.Tables) != 0 {
		t.Fatalf("impact after the retry = %+v", im)
	}
}

// The owner report reads each table's generation, entries and next step,
// the owner's down period and its overrides, also for an uninstalled
// owner whose rows are held.
func TestOwnerReport(t *testing.T) {
	clk := newFakeClock("2026-12-01T00:00:00Z")
	h := newHarness(t, openStore(t, ""), clk).load().run()
	authority := pusherOf(acmeAuthority, "push:node_holds")
	h.push(authority, extapi.KindAvailability, 3, backupFull)
	h.push(authority, extapi.KindNodeHolds, 1, opsHolds)
	if _, err := h.s.Override(context.Background(), OverrideRequest{Owner: "acme_authority", Kind: extapi.KindNodeHolds, Entry: "9",
		Action: OverrideDelete, Reason: "done", ByUser: 1}); err != nil {
		t.Fatal(err)
	}
	h.s.OwnerUp("acme_authority", 1)
	r := h.s.OwnerReport("acme_authority")
	if !r.Installed || !r.Verified || len(r.Tables) != 2 || !r.DownSince.IsZero() || !r.ClampAt.IsZero() || r.Grace != DefaultDownGrace ||
		len(r.Overrides) != 1 || r.Overrides[0].Entry != "9" {
		t.Fatalf("report while up = %+v", r)
	}
	av, holds := r.Tables[0], r.Tables[1]
	if av.Kind != extapi.KindAvailability || av.Generation != 3 || av.Entries != 1 || !av.Digest.Valid() ||
		!av.NextStep.Equal(time.Date(2027, 2, 1, 0, 0, 0, 0, time.UTC)) || av.PushedAt.IsZero() {
		t.Fatalf("availability report = %+v", av)
	}
	if holds.Kind != extapi.KindNodeHolds || holds.Entries != 2 || !holds.NextStep.IsZero() {
		t.Fatalf("holds report = %+v", holds)
	}
	h.s.OwnerDown("acme_authority")
	r = h.s.OwnerReport("acme_authority")
	if !r.DownSince.Equal(r.Now) || !r.ClampAt.Equal(r.DownSince.Add(DefaultDownGrace+BootExtension)) {
		t.Fatalf("report while down = %+v", r)
	}
	h.s.SetPackages([]Package{acmeBackup})
	if r := h.s.OwnerReport("acme_authority"); r.Installed || len(r.Tables) != 2 {
		t.Fatalf("report of an uninstalled owner with held rows = %+v", r)
	}
	if r := h.s.OwnerReport("acme_nobody"); r.Installed || len(r.Tables)+len(r.Overrides) != 0 {
		t.Fatalf("report of an unknown owner = %+v", r)
	}
}

// An owner that went down just before its uninstall, in the same commit of
// the writer, keeps no down record: the uninstall's delete is not followed
// by the down row.
func TestAnUninstallInTheTickOfTheOwnersDownLeavesNoDownRecord(t *testing.T) {
	st := openStore(t, "")
	h := newHarness(t, st, newFakeClock("2026-12-01T00:00:00Z")).load().run()
	h.push(pusherOf(acmeOps, "push:node_holds"), extapi.KindNodeHolds, 1, opsHolds)
	h.s.OwnerUp("acme_ops", 1)
	h.stop()
	h.s.OwnerDown("acme_ops")
	h.s.SetPackages([]Package{acmeAuthority, acmeBackup})
	done := make(chan error, 1)
	go func() { done <- h.s.Uninstall(context.Background(), "acme_ops") }()
	h.waitFor("the uninstall queued beside the down record", func() bool {
		h.s.mu.Lock()
		defer h.s.mu.Unlock()
		return len(h.s.ops) == 1 && h.s.downDirty["acme_ops"]
	})
	h.run()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	snap, err := st.LoadPushedState(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	for _, d := range snap.Down {
		if d.Owner == "acme_ops" {
			t.Fatalf("the uninstall left the down record %+v", d)
		}
	}
}
