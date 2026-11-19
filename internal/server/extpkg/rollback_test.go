package extpkg_test

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/extpkg"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/testenv/extsign"
	"gpuworks.ai/gpuworks/pkg/extapi"
)

func storage(v int) func(*extsign.Spec) {
	return func(s *extsign.Spec) {
		s.Manifest.Storage = &extapi.StorageDecl{Kind: extapi.StorageSQLite, HA: extapi.StorageHAShipped, StateVersion: v}
	}
}

func codeOnly(err error) string {
	code, _ := codeOf(err)
	return code
}

// upgradeTo upgrades the installed acme_backup to raw with the approval its
// refusal names.
func upgradeTo(t *testing.T, h *extpkg.Host, raw []byte, opt extpkg.UpgradeOptions) (*extpkg.Upgraded, error) {
	t.Helper()
	_, err := h.Upgrade(bytes.NewReader(raw), opt)
	_, need := approvalOf(t, err)
	opt.Approve = need.Hash
	return h.Upgrade(bytes.NewReader(raw), opt)
}

func writeData(t *testing.T, h *extpkg.Host, body string) {
	t.Helper()
	if err := os.MkdirAll(h.Dir.Data("acme_backup"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.Dir.Data("acme_backup"), "state"), []byte(body), 0o600); err != nil {
		t.Fatal(err)
	}
}

func dataOf(h *extpkg.Host) string {
	raw, _ := os.ReadFile(filepath.Join(h.Dir.Data("acme_backup"), "state"))
	return string(raw)
}

// The upgrade's steps run in order: the marker is written before the
// running version is quiesced, the data is copied with the data marker
// after that, and the new version becomes current last; an install of the
// same id is refused while the marker stands.
func TestUpgradeWritesTheMarkerQuiescesThenSnapshots(t *testing.T) {
	w := newWorld(t)
	h, mk := newHost(t, w)
	if _, err := h.Install(bytes.NewReader(pkg(t, w, "1.0.0", storage(1))), extpkg.InstallOptions{Enable: true}); err != nil {
		t.Fatal(err)
	}
	mk.markers[extapi.PluginDataMarker("acme_backup")] = extapi.Marker{Name: extapi.PluginDataMarker("acme_backup"), Version: 1, WrittenBy: "1.0.0"}
	writeData(t, h, "v1 rows")
	var order []string
	up, err := upgradeTo(t, h, pkg(t, w, "2.0.0", storage(2)), extpkg.UpgradeOptions{Marker: true, Snapshot: true, Quiesce: func() error {
		_, marked, _ := h.Dir.ReadUpgrade("acme_backup")
		current, _ := h.Dir.Current("acme_backup")
		list, _ := h.Dir.ListSnapshots("acme_backup")
		order = append(order, "quiesce", map[bool]string{true: "marked", false: "unmarked"}[marked], current, map[bool]string{true: "no snapshot", false: "snapshot"}[len(list) == 0])
		return nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	if got := order; len(got) != 4 || got[1] != "marked" || got[2] != "1.0.0" || got[3] != "no snapshot" {
		t.Fatalf("at quiesce: %v", got)
	}
	if up.Snapshot.From != "1.0.0" || up.Snapshot.Marker != 1 || up.Snapshot.MarkerBy != "1.0.0" {
		t.Fatalf("snapshot = %+v", up.Snapshot)
	}
	u, ok, err := h.Dir.ReadUpgrade("acme_backup")
	if err != nil || !ok || u.From != "1.0.0" || u.To != "2.0.0" || u.Snapshot != up.Snapshot.Name {
		t.Fatalf("marker = %+v %v %v", u, ok, err)
	}
	_, err = h.Install(bytes.NewReader(pkg(t, w, "3.0.0", storage(2))), extpkg.InstallOptions{})
	if codeOnly(err) != messages.ErrorPluginUpgradeInProgress {
		t.Fatalf("install during an upgrade = %v", err)
	}
	if err := h.FinishUpgrade("acme_backup"); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := h.Dir.ReadUpgrade("acme_backup"); ok {
		t.Fatal("FinishUpgrade left the marker")
	}
}

// A quiesce or snapshot that fails refuses the upgrade, removes the marker
// and leaves the old version current; a failed copy is
// PLUGIN_SNAPSHOT_FAILED.
func TestAFailedSnapshotRefusesTheUpgrade(t *testing.T) {
	w := newWorld(t)
	h, _ := newHost(t, w)
	if _, err := h.Install(bytes.NewReader(pkg(t, w, "1.0.0", nil)), extpkg.InstallOptions{Enable: true}); err != nil {
		t.Fatal(err)
	}
	raw := pkg(t, w, "2.0.0", nil)
	_, err := upgradeTo(t, h, raw, extpkg.UpgradeOptions{Marker: true, Snapshot: true, Quiesce: func() error { return errors.New("did not stop") }})
	if err == nil || err.Error() != "did not stop" {
		t.Fatalf("upgrade with a failed quiesce = %v", err)
	}
	// A file where the snapshots directory belongs: the copy cannot be made.
	if err := os.Remove(h.Dir.Snapshots("acme_backup")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(h.Dir.Snapshots("acme_backup"), nil, 0o600); err != nil {
		t.Fatal(err)
	}
	_, err = upgradeTo(t, h, raw, extpkg.UpgradeOptions{Marker: true, Snapshot: true})
	var pe *messages.ProductError
	if !errors.As(err, &pe) || pe.Code() != messages.ErrorPluginSnapshotFailed || pe.Params()["id"] != "acme_backup" {
		t.Fatalf("upgrade with a failed snapshot = %v", err)
	}
	if v, _ := h.Dir.Current("acme_backup"); v != "1.0.0" {
		t.Fatalf("current = %s", v)
	}
	if _, ok, _ := h.Dir.ReadUpgrade("acme_backup"); ok {
		t.Fatal("a refused upgrade left its marker")
	}
}

// A rollback without the data is refused when the data marker is newer
// than the older build, and nothing changes; with the data it restores the
// snapshot, puts the marker back and makes the older version current with
// its own approval hash; with nothing older it is
// PLUGIN_NO_PREVIOUS_VERSION, and a restore without a snapshot of that
// version is PLUGIN_SNAPSHOT_MISSING.
func TestRollbackGuardsAndRestores(t *testing.T) {
	w := newWorld(t)
	h, mk := newHost(t, w)
	v1 := pkg(t, w, "1.0.0", storage(1))
	in, err := h.Install(bytes.NewReader(v1), extpkg.InstallOptions{Enable: true, ApprovedBy: "root"})
	if err != nil {
		t.Fatal(err)
	}
	name := extapi.PluginDataMarker("acme_backup")
	if _, err := h.Rollback("acme_backup", extpkg.RollbackOptions{}); codeOnly(err) != messages.ErrorPluginNoPreviousVersion {
		t.Fatalf("rollback of a first install = %v", err)
	}
	mk.markers[name] = extapi.Marker{Name: name, Version: 1, WrittenBy: "1.0.0"}
	writeData(t, h, "v1 rows")
	if _, err := upgradeTo(t, h, pkg(t, w, "2.0.0", storage(2)), extpkg.UpgradeOptions{Snapshot: true, ApprovedBy: "ops"}); err != nil {
		t.Fatal(err)
	}
	mk.markers[name] = extapi.Marker{Name: name, Version: 2, WrittenBy: "2.0.0"}
	writeData(t, h, "v2 rows")

	quiesced := false
	_, err = h.Rollback("acme_backup", extpkg.RollbackOptions{By: "ops", Quiesce: func() error { quiesced = true; return nil }})
	if codeOnly(err) != extapi.ErrorStateTooNew || quiesced {
		t.Fatalf("rollback over newer data = %v (quiesced %v)", err, quiesced)
	}
	if v, _ := h.Dir.Current("acme_backup"); v != "2.0.0" || dataOf(h) != "v2 rows" {
		t.Fatalf("after the refused rollback: %s %q", v, dataOf(h))
	}
	if _, err := h.Rollback("acme_backup", extpkg.RollbackOptions{Restore: true, Snapshot: "20990101T000000.000000000Z-1.0.0"}); codeOnly(err) != messages.ErrorPluginSnapshotMissing {
		t.Fatalf("restore of a snapshot that does not exist = %v", err)
	}

	rb, err := h.Rollback("acme_backup", extpkg.RollbackOptions{Restore: true, By: "ops", Quiesce: func() error { quiesced = true; return nil }})
	if err != nil {
		t.Fatal(err)
	}
	if !quiesced || rb.From != "2.0.0" || rb.Manifest.Version != "1.0.0" || rb.Restored == "" {
		t.Fatalf("rolled back = %+v (quiesced %v)", rb, quiesced)
	}
	if v, _ := h.Dir.Current("acme_backup"); v != "1.0.0" || dataOf(h) != "v1 rows" {
		t.Fatalf("after the rollback: %s %q", v, dataOf(h))
	}
	if m := mk.markers[name]; m.Version != 1 || m.WrittenBy != "1.0.0" {
		t.Fatalf("data marker = %+v", m)
	}
	ctl, _, _ := h.Dir.ReadControl("acme_backup")
	if !ctl.Enabled || ctl.ApprovedHash != string(in.ApprovalHash) || ctl.ApprovedBy != "ops" {
		t.Fatalf("control = %+v, want 1.0.0's approval %s", ctl, in.ApprovalHash)
	}
	if _, ok, _ := h.Dir.ReadUpgrade("acme_backup"); ok {
		t.Fatal("the rollback left an upgrade marker")
	}
	if _, err := h.Rollback("acme_backup", extpkg.RollbackOptions{}); codeOnly(err) != messages.ErrorPluginNoPreviousVersion {
		t.Fatalf("rollback with nothing older = %v", err)
	}
}

// Snapshot data without a data marker is restored by deleting the marker:
// the restored data was written before any build recorded one.
func TestRestoringASnapshotWithoutAMarkerDeletesIt(t *testing.T) {
	w := newWorld(t)
	h, mk := newHost(t, w)
	if _, err := h.Install(bytes.NewReader(pkg(t, w, "1.0.0", storage(1))), extpkg.InstallOptions{Enable: true}); err != nil {
		t.Fatal(err)
	}
	if _, err := upgradeTo(t, h, pkg(t, w, "2.0.0", storage(2)), extpkg.UpgradeOptions{Snapshot: true}); err != nil {
		t.Fatal(err)
	}
	name := extapi.PluginDataMarker("acme_backup")
	mk.markers[name] = extapi.Marker{Name: name, Version: 2, WrittenBy: "2.0.0"}
	if _, err := h.Rollback("acme_backup", extpkg.RollbackOptions{Restore: true}); err != nil {
		t.Fatal(err)
	}
	if _, ok := mk.markers[name]; ok {
		t.Fatalf("data marker after restoring a snapshot taken without one = %+v", mk.markers[name])
	}
}
