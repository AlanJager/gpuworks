//go:build unix

package layout_test

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/extpkg/layout"
)

// An upgrade copies its package's data without the directory's lock, so
// Cleanup leaves the partial snapshots of a package with an upgrade marker
// alone and removes those of any other; removing the marker removes them.
func TestCleanupLeavesTheSnapshotAnUpgradeIsTaking(t *testing.T) {
	d := layout.New(t.TempDir())
	for _, id := range []string{"acme_backup", "acme_driver"} {
		if err := os.MkdirAll(filepath.Join(d.Snapshots(id), "20261120T000000.000000000Z-1.0.0.partial"), 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := d.WriteUpgrade("acme_backup", layout.Upgrade{From: "1.0.0", To: "2.0.0", Snapshot: "s", Started: time.Now()}); err != nil {
		t.Fatal(err)
	}
	if err := d.Cleanup(); err != nil {
		t.Fatal(err)
	}
	partial := func(id string) bool {
		_, err := os.Stat(filepath.Join(d.Snapshots(id), "20261120T000000.000000000Z-1.0.0.partial"))
		return err == nil
	}
	if !partial("acme_backup") || partial("acme_driver") {
		t.Fatalf("after Cleanup: upgrading package's partial kept %v, other's kept %v", partial("acme_backup"), partial("acme_driver"))
	}
	if err := d.RemoveUpgrade("acme_backup"); err != nil {
		t.Fatal(err)
	}
	if partial("acme_backup") {
		t.Fatal("RemoveUpgrade left the partial snapshot")
	}
}
