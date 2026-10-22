package native

import (
	"context"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"
)

func singleLayerImage(t *testing.T) (*Engine, string, string) {
	t.Helper()
	layer := buildLayer(t, true, []entry{{name: "etc", dir: true}, {name: "etc/model", body: "ok"}})
	reg := newFakeRegistry(t, "team/base", [][]byte{layer}, []byte(`{"architecture":"amd64","os":"linux"}`))
	engine, dir := engineFor(t, reg, 0)
	t.Cleanup(engine.WaitBackground)
	if err := engine.Pull(context.Background(), reg.image(), nil); err != nil {
		t.Fatal(err)
	}
	return engine, dir, reg.image()
}

func createLines(t *testing.T, engine *Engine, name, image string) []string {
	t.Helper()
	var lines []string
	if err := engine.Create(context.Background(), name, image, func(line string) { lines = append(lines, line) }); err != nil {
		t.Fatal(err)
	}
	return lines
}

func mentions(lines []string, text string) bool {
	return slices.ContainsFunc(lines, func(line string) bool { return strings.Contains(line, text) })
}

// Without reflink a clone is a full copy, slower than unpacking: no base is
// ever built, and one left by an earlier version is reclaimed.
func TestCreateKeepsNoBaseWithoutReflink(t *testing.T) {
	engine, dir, image := singleLayerImage(t)
	engine.Reflink = func(string) bool { return false }
	stale := filepath.Join(dir, "base", "stale-from-an-older-agent", "ROOT")
	if err := os.MkdirAll(stale, 0o755); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"c1", "c2"} {
		lines := createLines(t, engine, name, image)
		engine.WaitBackground()
		if mentions(lines, "reused immutable base") || mentions(lines, "building a reusable base") {
			t.Fatalf("%s used a base cache without reflink: %v", name, lines)
		}
		if data, err := os.ReadFile(filepath.Join(dir, "containers", name, "ROOT", "etc", "model")); err != nil || string(data) != "ok" {
			t.Fatalf("%s rootfs: %q %v", name, data, err)
		}
	}
	if _, err := os.Stat(filepath.Join(dir, "base")); !os.IsNotExist(err) {
		t.Fatalf("base directory kept without reflink: %v", err)
	}
}

// With reflink the first create returns without building the base (building
// it synchronously forced the fresh tree to disk first); the base appears in
// the background and later creates clone it.
func TestCreateBuildsBaseInBackgroundWithReflink(t *testing.T) {
	engine, dir, image := singleLayerImage(t)
	engine.Reflink = func(string) bool { return true }
	first := createLines(t, engine, "c1", image)
	if !mentions(first, "building a reusable base rootfs in the background") || mentions(first, "reused immutable base") {
		t.Fatalf("first create progress: %v", first)
	}
	engine.WaitBackground()
	id, _ := engine.Layout.ImageID(ParseRef(image))
	base := engine.Layout.baseDir(id)
	if !isCompleteBase(filepath.Join(base, "ROOT")) {
		t.Fatal("background base was not published")
	}
	if _, err := os.Stat(base + ".building"); !os.IsNotExist(err) {
		t.Fatalf("building directory left behind: %v", err)
	}
	second := createLines(t, engine, "c2", image)
	if !mentions(second, "reused immutable base rootfs") || mentions(second, "building a reusable base") {
		t.Fatalf("second create progress: %v", second)
	}
	if data, err := os.ReadFile(filepath.Join(dir, "containers", "c2", "ROOT", "etc", "model")); err != nil || string(data) != "ok" {
		t.Fatalf("cloned rootfs: %q %v", data, err)
	}
}

// A base whose image disappears while it is being built is discarded rather
// than published for an image nothing can create any more.
func TestBaseBuildDiscardedWhenImageRemoved(t *testing.T) {
	engine, _, image := singleLayerImage(t)
	ref := ParseRef(image)
	id, _ := engine.Layout.ImageID(ref)
	manifest, _ := engine.Layout.LoadManifest(ref)
	var layers []LayerSource
	for _, desc := range manifest.Layers {
		path := filepath.Join(engine.Layout.LayersDir(), desc.Digest)
		layers = append(layers, LayerSource{Digest: desc.Digest, Size: desc.Size,
			Open: func(context.Context) (io.ReadCloser, error) { return os.Open(path) }})
	}
	// Unregister the tag but keep the blobs, as a concurrent RemoveImage
	// would have done by the time the build reaches its final check.
	if err := os.RemoveAll(engine.Layout.TagDir(ref)); err != nil {
		t.Fatal(err)
	}
	if err := engine.buildBase(context.Background(), id, image, layers); err == nil || !strings.Contains(err.Error(), "was removed") {
		t.Fatalf("build for a removed image: %v", err)
	}
	for _, path := range []string{engine.Layout.baseDir(id), engine.Layout.baseDir(id) + ".building"} {
		if _, err := os.Stat(path); !os.IsNotExist(err) {
			t.Fatalf("%s left behind: %v", path, err)
		}
	}
}

func TestReflinkProbeLeavesNothingBehind(t *testing.T) {
	dir := t.TempDir()
	_ = ReflinkSupported(dir) // the answer depends on the host filesystem
	if entries, _ := os.ReadDir(dir); len(entries) != 0 {
		t.Fatalf("probe left files: %v", entries)
	}
}
