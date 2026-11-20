package extpkg_test

import (
	"bytes"
	"errors"
	"os"
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/extpkg"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/testenv/extsign"
)

// A version unpacked before with other contents is refused before the
// running version is quiesced: nothing is stopped, no snapshot is taken
// (so none of the kept ones is pruned) and no marker is left.
func TestAVersionConflictRefusesBeforeQuiescing(t *testing.T) {
	w := newWorld(t)
	h, _ := newHost(t, w)
	if _, err := h.Install(bytes.NewReader(pkg(t, w, "1.0.0", nil)), extpkg.InstallOptions{Enable: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := upgradeTo(t, h, pkg(t, w, "2.0.0", nil), extpkg.UpgradeOptions{Marker: true, Snapshot: true}); err != nil {
		t.Fatal(err)
	}
	if err := h.FinishUpgrade("acme_backup"); err != nil {
		t.Fatal(err)
	}
	if _, err := h.Rollback("acme_backup", extpkg.RollbackOptions{By: "ops"}); err != nil {
		t.Fatal(err)
	}
	before, _ := h.Dir.ListSnapshots("acme_backup")
	quiesced := false
	other := pkg(t, w, "2.0.0", func(s *extsign.Spec) { s.Files["locales/en.json"] = extsign.File{Body: []byte(`{"title":"other"}`)} })
	_, err := upgradeTo(t, h, other, extpkg.UpgradeOptions{Marker: true, Snapshot: true, Quiesce: func() error { quiesced = true; return nil }})
	if codeOnly(err) != messages.ErrorPluginVersionConflict {
		t.Fatalf("upgrade to other contents of 2.0.0 = %v", err)
	}
	if quiesced {
		t.Fatal("a version conflict stopped the running version")
	}
	if after, _ := h.Dir.ListSnapshots("acme_backup"); !slices.Equal(after, before) {
		t.Fatalf("snapshots changed: %v -> %v", before, after)
	}
	if _, ok, _ := h.Dir.ReadUpgrade("acme_backup"); ok {
		t.Fatal("a refused upgrade left its marker")
	}
	if v, _ := h.Dir.Current("acme_backup"); v != "1.0.0" {
		t.Fatalf("current = %s", v)
	}
}

// The running version is quiesced with no lock of the data directory
// held, so another package's administration goes on meanwhile; an upgrade
// refused after it unpacked the new version removes that version again.
func TestQuiesceHoldsNoDirectoryLock(t *testing.T) {
	w := newWorld(t)
	h, _ := newHost(t, w)
	if _, err := h.Install(bytes.NewReader(pkg(t, w, "1.0.0", nil)), extpkg.InstallOptions{Enable: true}); err != nil {
		t.Fatal(err)
	}
	driver := extsign.Package(t, w.pub, extsign.Minimal("acme", "acme_driver", "1.0.0"))
	if _, err := h.Install(bytes.NewReader(driver), extpkg.InstallOptions{Enable: true}); err != nil {
		t.Fatal(err)
	}
	_, err := upgradeTo(t, h, pkg(t, w, "2.0.0", nil), extpkg.UpgradeOptions{Marker: true, Snapshot: true, Quiesce: func() error {
		unlock, free, err := h.Dir.TryLock()
		if err != nil || !free {
			t.Errorf("the directory lock is held while quiescing (%v)", err)
			return errors.New("locked")
		}
		_ = unlock()
		if err := h.SetEnabled("acme_driver", false); err != nil {
			t.Errorf("disable another package while quiescing: %v", err)
		}
		return errors.New("did not stop")
	}})
	if err == nil || err.Error() != "did not stop" {
		t.Fatalf("upgrade = %v", err)
	}
	if ctl, _, _ := h.Dir.ReadControl("acme_driver"); ctl.Enabled {
		t.Fatal("the other package was not disabled")
	}
	if versions, _ := h.Dir.Versions("acme_backup"); !slices.Equal(versions, []string{"1.0.0"}) {
		t.Fatalf("versions after the refused upgrade = %v", versions)
	}
}

// An upgrade that fails after the new version became current says so
// (Activated, From) and keeps its marker, so the caller, or the next
// boot, rolls back.
func TestAFailureAfterActivationKeepsTheMarker(t *testing.T) {
	w := newWorld(t)
	h, _ := newHost(t, w)
	if _, err := h.Install(bytes.NewReader(pkg(t, w, "1.0.0", nil)), extpkg.InstallOptions{Enable: true}); err != nil {
		t.Fatal(err)
	}
	control := h.Dir.Package("acme_backup") + "/control.json"
	up, err := upgradeTo(t, h, pkg(t, w, "2.0.0", nil), extpkg.UpgradeOptions{Marker: true, Snapshot: true, Quiesce: func() error {
		// A directory where the control file belongs: it cannot be written.
		if err := os.Remove(control); err != nil {
			return err
		}
		return os.Mkdir(control, 0o700)
	}})
	if err == nil || up == nil || !up.Activated || up.From != "1.0.0" || up.Manifest.Version != "2.0.0" || up.Snapshot.From != "1.0.0" {
		t.Fatalf("upgrade = %+v %v, want activated with an error", up, err)
	}
	if v, _ := h.Dir.Current("acme_backup"); v != "2.0.0" {
		t.Fatalf("current = %s", v)
	}
	u, ok, err := h.Dir.ReadUpgrade("acme_backup")
	if err != nil || !ok || u.From != "1.0.0" || u.To != "2.0.0" {
		t.Fatalf("marker = %+v %v %v", u, ok, err)
	}
}
