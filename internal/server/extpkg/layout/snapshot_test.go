//go:build unix

package layout_test

import (
	"net"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/extpkg/layout"
)

func writeData(t *testing.T, d layout.Dir, id string, files map[string]string) {
	t.Helper()
	for name, body := range files {
		p := filepath.Join(d.Data(id), name)
		if err := os.MkdirAll(filepath.Dir(p), 0o700); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0o600); err != nil {
			t.Fatal(err)
		}
	}
}

func readTree(t *testing.T, dir string) map[string]string {
	t.Helper()
	out := map[string]string{}
	err := filepath.Walk(dir, func(p string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(dir, p)
		switch {
		case info.Mode()&os.ModeSymlink != 0:
			link, _ := os.Readlink(p)
			out[rel] = "-> " + link
		case info.Mode().IsRegular():
			body, err := os.ReadFile(p)
			if err != nil {
				return err
			}
			out[rel] = string(body)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return out
}

// A snapshot copies data whole (files, directories and links as links,
// not sockets), records what it was taken from, and a restore puts exactly
// that copy back over data that changed since.
func TestSnapshotAndRestoreTheDataDirectory(t *testing.T) {
	d := layout.New(t.TempDir())
	writeData(t, d, "acme_x", map[string]string{"state.db": "v1 rows", "sub/notes": "kept"})
	if err := os.Symlink("state.db", filepath.Join(d.Data("acme_x"), "current")); err != nil {
		t.Fatal(err)
	}
	sock := filepath.Join(d.Data("acme_x"), "s.sock")
	l, err := net.Listen("unix", sock)
	if err != nil {
		t.Fatal(err)
	}
	defer l.Close()
	at := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	s, err := d.TakeSnapshot("acme_x", layout.Snapshot{From: "1.0.0", Marker: 1, MarkerBy: "1.0.0", Taken: at})
	if err != nil {
		t.Fatal(err)
	}
	if s.Name != "20261119T100000.000000000Z-1.0.0" {
		t.Fatalf("name = %q", s.Name)
	}
	got, ok, err := d.FindSnapshot("acme_x", s.Name)
	if err != nil || !ok || got.From != "1.0.0" || got.Marker != 1 || got.MarkerBy != "1.0.0" || !got.Taken.Equal(at) {
		t.Fatalf("snapshot = %+v %v %v", got, ok, err)
	}
	writeData(t, d, "acme_x", map[string]string{"state.db": "v2 rows", "v2-only": "new"})
	if err := d.Restore("acme_x", s.Name); err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"state.db": "v1 rows", filepath.Join("sub", "notes"): "kept", "current": "-> state.db"}
	if tree := readTree(t, d.Data("acme_x")); !mapsEqual(tree, want) {
		t.Fatalf("restored data = %v, want %v", tree, want)
	}
	if info, err := os.Stat(d.Data("acme_x")); err != nil || info.Mode().Perm() != 0o700 {
		t.Fatalf("restored data dir = %v %v", info, err)
	}
	if left, _ := filepath.Glob(filepath.Join(d.Package("acme_x"), ".data-*")); len(left) != 0 {
		t.Fatalf("restore left %v", left)
	}
}

// Only the newest two snapshots are kept, and a partial one is never
// listed and goes at cleanup.
func TestSnapshotRetentionAndPartialCleanup(t *testing.T) {
	d := layout.New(t.TempDir())
	writeData(t, d, "acme_x", map[string]string{"f": "x"})
	base := time.Date(2026, 11, 19, 10, 0, 0, 0, time.UTC)
	for i, v := range []string{"1.0.0", "1.1.0", "1.2.0"} {
		if _, err := d.TakeSnapshot("acme_x", layout.Snapshot{From: v, Taken: base.Add(time.Duration(i) * time.Minute)}); err != nil {
			t.Fatal(err)
		}
	}
	partial := filepath.Join(d.Snapshots("acme_x"), layout.SnapshotName("1.3.0", base.Add(time.Hour))+".partial")
	if err := os.MkdirAll(partial, 0o700); err != nil {
		t.Fatal(err)
	}
	list, err := d.ListSnapshots("acme_x")
	if err != nil {
		t.Fatal(err)
	}
	var from []string
	for _, s := range list {
		from = append(from, s.From)
	}
	if !slices.Equal(from, []string{"1.1.0", "1.2.0"}) {
		t.Fatalf("kept %v, want the newest two", from)
	}
	if err := d.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(partial); !os.IsNotExist(err) {
		t.Fatalf("partial snapshot after cleanup: %v", err)
	}
	if _, err := d.TakeSnapshot("acme_x", layout.Snapshot{From: "1.2.0", Taken: base.Add(2 * time.Minute)}); err == nil {
		t.Fatal("a snapshot replaced one of the same name")
	}
}

// A restore interrupted after it moved data aside is undone by cleanup:
// the displaced data comes back.
func TestCleanupMovesBackDataAnInterruptedRestoreDisplaced(t *testing.T) {
	d := layout.New(t.TempDir())
	writeData(t, d, "acme_x", map[string]string{"f": "original"})
	displaced := filepath.Join(d.Package("acme_x"), ".data-displaced.partial")
	building := filepath.Join(d.Package("acme_x"), ".data-restore.partial")
	if err := os.Rename(d.Data("acme_x"), displaced); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(building, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := d.Cleanup(); err != nil {
		t.Fatal(err)
	}
	if tree := readTree(t, d.Data("acme_x")); tree["f"] != "original" {
		t.Fatalf("data after cleanup = %v", tree)
	}
	for _, p := range []string{displaced, building} {
		if _, err := os.Stat(p); !os.IsNotExist(err) {
			t.Fatalf("%s after cleanup: %v", p, err)
		}
	}
}

// The upgrade marker round-trips, is listed, refuses unknown fields and
// goes with an uninstall that keeps the data and snapshots.
func TestUpgradeMarker(t *testing.T) {
	d := layout.New(t.TempDir())
	if _, ok, err := d.ReadUpgrade("acme_x"); ok || err != nil {
		t.Fatalf("missing marker = %v %v", ok, err)
	}
	want := layout.Upgrade{From: "1.0.0", To: "2.0.0", Snapshot: "s", Started: time.Date(2026, 11, 19, 0, 0, 0, 0, time.UTC)}
	if err := d.WriteUpgrade("acme_x", want); err != nil {
		t.Fatal(err)
	}
	raw, _ := os.ReadFile(filepath.Join(d.Package("acme_x"), "upgrade.json"))
	if string(raw) != "{\n  \"from\": \"1.0.0\",\n  \"to\": \"2.0.0\",\n  \"snapshot\": \"s\",\n  \"started\": \"2026-11-19T00:00:00Z\"\n}\n" {
		t.Fatalf("upgrade.json = %s", raw)
	}
	if got, ok, err := d.ReadUpgrade("acme_x"); !ok || err != nil || got != want {
		t.Fatalf("marker = %+v %v %v", got, ok, err)
	}
	if ids, err := d.Upgrading(); err != nil || !slices.Equal(ids, []string{"acme_x"}) {
		t.Fatalf("upgrading = %v %v", ids, err)
	}
	writeData(t, d, "acme_x", map[string]string{"f": "x"})
	if _, err := d.TakeSnapshot("acme_x", layout.Snapshot{From: "1.0.0", Taken: want.Started}); err != nil {
		t.Fatal(err)
	}
	if err := d.Remove("acme_x", false, time.Now()); err != nil {
		t.Fatal(err)
	}
	if _, ok, _ := d.ReadUpgrade("acme_x"); ok {
		t.Fatal("the upgrade marker survived the uninstall")
	}
	if list, _ := d.ListSnapshots("acme_x"); len(list) != 1 {
		t.Fatalf("snapshots after an uninstall without purge = %v", list)
	}
	if err := os.WriteFile(filepath.Join(d.Package("acme_x"), "upgrade.json"), []byte(`{"from":"1","extra":1}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, _, err := d.ReadUpgrade("acme_x"); err == nil {
		t.Fatal("a marker with an unknown field was read")
	}
}

func mapsEqual(a, b map[string]string) bool {
	if len(a) != len(b) {
		return false
	}
	for k, v := range a {
		if b[k] != v {
			return false
		}
	}
	return true
}
