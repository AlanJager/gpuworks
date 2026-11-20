//go:build linux || darwin

package pluginbridge

import (
	"bytes"
	"context"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/extpkg/layout"
	"gpuworks.ai/gpuworks/internal/server/pluginhost"
	"gpuworks.ai/gpuworks/internal/testenv/extsign"
)

// An install unpacks its package under the directory's lock but makes it
// current only under the lock of its id, so it cannot slip in under a
// rollback, restart or uninstall of the same id.
func TestInstallMakesItsPackageCurrentUnderTheIDLock(t *testing.T) {
	w := newWorld(t, nil)
	b, _ := w.openUpgrades(&upgradeSpawner{}, time.Hour)
	install(t, b, w.pkg("acme_backup", "1.0.0", nil), true)
	raw := w.pkg("acme_backup", "2.0.0", nil)
	_, err := b.Install(context.Background(), bytes.NewReader(raw), InstallRequest{By: "admin"})
	hash := approvalOf(t, err)

	unlock := b.locks.lock("acme_backup")
	done := make(chan error, 1)
	go func() {
		_, err := b.Install(context.Background(), bytes.NewReader(raw), InstallRequest{Approve: hash, By: "admin"})
		done <- err
	}()
	// The install must still be waiting for the id after it had ample
	// time to unpack.
	deadline := time.Now().Add(300 * time.Millisecond)
	for time.Now().Before(deadline) {
		if v, _ := b.pkgs.Dir.Current("acme_backup"); v != "1.0.0" {
			unlock()
			t.Fatalf("the install made %s current while the id was held", v)
		}
		select {
		case err := <-done:
			unlock()
			t.Fatalf("the install returned while the id was held: %v", err)
		case <-time.After(10 * time.Millisecond):
		}
	}
	unlock()
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if v, _ := b.pkgs.Dir.Current("acme_backup"); v != "2.0.0" {
		t.Fatalf("current after the install = %s", v)
	}
}

// A restoring rollback stopped after it made the older version current
// and before it recorded that version's approval is finished at the next
// boot: the older version's own approval hash is recorded and it runs.
func TestBootFinishesARollbackStoppedBeforeItsApproval(t *testing.T) {
	w := newWorld(t, nil)
	sp := &upgradeSpawner{}
	b, stop := w.openUpgrades(sp, 100*time.Millisecond)
	install(t, b, w.pkg("acme_backup", "1.0.0", nil), true)
	v1, _, err := b.pkgs.Dir.ReadControl("acme_backup")
	if err != nil {
		t.Fatal(err)
	}
	// 2.0.0 asks for more, so its approval hash differs from 1.0.0's.
	banners := func(s *extsign.Spec) {
		s.Manifest.Permissions = []string{"push:banners"}
		s.Manifest.Uses = map[string]int{"table.banners": 1}
	}
	if _, err := upgrade(t, b, "acme_backup", w.pkg("acme_backup", "2.0.0", banners)); err != nil {
		t.Fatal(err)
	}
	stop()

	// What the disk holds when the server dies inside the rollback.
	snaps, err := b.pkgs.Dir.ListSnapshots("acme_backup")
	if err != nil || len(snaps) != 1 {
		t.Fatalf("snapshots = %v %v", snaps, err)
	}
	if err := b.pkgs.Dir.WriteUpgrade("acme_backup", layout.Upgrade{From: "1.0.0", To: "2.0.0", Snapshot: snaps[0].Name, Started: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if _, err := b.pkgs.Dir.Activate("acme_backup", "1.0.0"); err != nil {
		t.Fatal(err)
	}
	if ctl, _, _ := b.pkgs.Dir.ReadControl("acme_backup"); ctl.ApprovedHash == v1.ApprovedHash {
		t.Fatal("the control file already holds 1.0.0's approval")
	}

	b2, _ := w.openUpgrades(sp, time.Hour)
	ctl, _, err := b2.pkgs.Dir.ReadControl("acme_backup")
	if err != nil || ctl.ApprovedHash != v1.ApprovedHash || ctl.ApprovedBy != "boot recovery" {
		t.Fatalf("control after the boot = %+v %v, want 1.0.0's approval %s", ctl, err, v1.ApprovedHash)
	}
	if v := waitState(t, b2, "acme_backup", string(pluginhost.StateRunning)); v.Version != "1.0.0" {
		t.Fatalf("after the boot: %+v", v)
	}
	noUpgradeMarker(t, w, "acme_backup")
}
