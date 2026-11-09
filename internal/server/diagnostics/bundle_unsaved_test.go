package diagnostics

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

// TestACollectionThatCannotBeSavedStillShowsWhyItFailed: on a full disk the
// collection fails and so does the manifest saying so; the list must still
// show it, or the console shows nothing at all.
func TestACollectionThatCannotBeSavedStillShowsWhyItFailed(t *testing.T) {
	if runtime.GOOS == "windows" || os.Geteuid() == 0 {
		t.Skip("needs a directory this process cannot write")
	}
	m, _, _ := newManager(t)
	logFile := filepath.Join(t.TempDir(), "server.log")
	_ = os.WriteFile(logFile, nil, 0o600)
	m.SetSources(&fakeSources{logFile: logFile})
	if err := os.MkdirAll(m.BundleDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	// The directory refuses every new file, the way a full disk does.
	if err := os.Chmod(m.BundleDir(), 0o500); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chmod(m.BundleDir(), 0o700) })

	b, err := m.StartBundle(context.Background(), BundleRequest{RequestedBy: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	done := waitBundle(t, m, b.ID)
	if done.State != "failed" || !strings.Contains(done.Error, "permission denied") {
		t.Fatalf("collection = %+v, want failed with the write error", done)
	}
	list := m.Bundles()
	if len(list) != 1 || list[0].ID != b.ID || list[0].State != "failed" || list[0].Error == "" {
		t.Fatalf("list = %+v, want the failed collection and why", list)
	}

	// Once the disk takes writes again, the next manifest write of a
	// collection replaces the memory copy.
	if err := os.Chmod(m.BundleDir(), 0o700); err != nil {
		t.Fatal(err)
	}
	m.writeBundleManifest(done)
	if _, err := os.Stat(m.bundleManifest(b.ID)); err != nil {
		t.Fatalf("manifest after the disk recovers: %v", err)
	}
	m.mu.Lock()
	left := len(m.unsaved)
	m.mu.Unlock()
	if left != 0 {
		t.Fatalf("unsaved = %d after the manifest was written, want 0", left)
	}
	if got, ok := m.Bundle(b.ID); !ok || got.State != "failed" {
		t.Fatalf("Bundle = %+v %v, want the saved manifest", got, ok)
	}
}

func TestUnsavedCollectionsAreBounded(t *testing.T) {
	m, _, _ := newManager(t)
	// No bundle directory: every manifest write fails.
	m.SetBundleDir(filepath.Join(t.TempDir(), "missing", "bundles"))
	for i := 0; i < maxUnsavedBundles+5; i++ {
		id := "20261106-0000" + string(rune('a'+i))
		m.writeBundleManifest(Bundle{ID: id, State: "failed"})
	}
	if n := len(m.Bundles()); n != maxUnsavedBundles {
		t.Fatalf("listed = %d, want at most %d kept in memory", n, maxUnsavedBundles)
	}
}
