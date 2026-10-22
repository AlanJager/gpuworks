package native

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"
)

// twoImages pulls two images that share their base layer into one layout.
func twoImages(t *testing.T) (*Engine, string, *fakeRegistry, *fakeRegistry) {
	t.Helper()
	shared := buildLayer(t, true, []entry{{name: "etc", dir: true}, {name: "etc/os-release", body: "shared"}})
	regA := newFakeRegistry(t, "team/a", [][]byte{shared, buildLayer(t, true, []entry{{name: "a.txt", body: "a"}})},
		[]byte(`{"config":{"Entrypoint":["/bin/a"],"Cmd":["serve"],"Env":["A=1","PATH=/bin"],"Volumes":{"/data":{}}}}`))
	regB := newFakeRegistry(t, "team/b", [][]byte{shared, buildLayer(t, true, []entry{{name: "b.txt", body: "b"}})}, []byte(`{"config":{}}`))
	engine, dir := engineFor(t, regA, 0)
	// These tests cover base cleanup, so the cache must exist on any test fs.
	engine.Reflink = func(string) bool { return true }
	// Registered after TempDir's cleanup, so it runs first: no background
	// base build may still be writing when the directory is removed.
	t.Cleanup(engine.WaitBackground)
	for _, reg := range []*fakeRegistry{regA, regB} {
		if err := engine.Pull(context.Background(), reg.image(), nil); err != nil {
			t.Fatal(err)
		}
	}
	return engine, dir, regA, regB
}

func blobExists(t *testing.T, dir, digest string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, "layers", digest))
	return err == nil
}

func TestLoadImageConfig(t *testing.T) {
	engine, _, regA, _ := twoImages(t)
	config, err := engine.Layout.LoadImageConfig(ParseRef(regA.image()))
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(config.Entrypoint, []string{"/bin/a"}) || !slices.Equal(config.Cmd, []string{"serve"}) ||
		!slices.Equal(config.Env, []string{"A=1", "PATH=/bin"}) || !slices.Equal(config.Volumes, []string{"/data"}) {
		t.Fatalf("config %+v", config)
	}
	if _, err := engine.Layout.LoadImageConfig(ParseRef("missing/image:tag")); err == nil {
		t.Fatal("config of an unregistered image")
	}
}

func TestTagImageSharesBlobs(t *testing.T) {
	engine, dir, regA, _ := twoImages(t)
	target := ParseRef("canonical.example/team/a:v1")
	if err := engine.Layout.TagImage(ParseRef(regA.image()), target); err != nil {
		t.Fatal(err)
	}
	if !engine.Layout.HasImage(target) {
		t.Fatal("tagged image not complete")
	}
	srcID, _ := engine.Layout.ImageID(ParseRef(regA.image()))
	dstID, _ := engine.Layout.ImageID(target)
	if srcID != dstID {
		t.Fatalf("tag changed the manifest identity: %s != %s", srcID, dstID)
	}
	// The new name must be creatable like any other.
	if err := engine.Create(context.Background(), "from-tag", "canonical.example/team/a:v1", nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "containers", "from-tag", "ROOT", "a.txt")); err != nil {
		t.Fatal(err)
	}
	if err := engine.Layout.TagImage(ParseRef("missing/image:tag"), target); err == nil {
		t.Fatal("tagging an unregistered image succeeded")
	}
}

func TestRemoveImageKeepsSharedAndLegacyBlobs(t *testing.T) {
	engine, dir, regA, regB := twoImages(t)
	refA := ParseRef(regA.image())
	manifestA, _ := engine.Layout.LoadManifest(refA)
	shared, onlyA := manifestA.Layers[0].Digest, manifestA.Layers[1].Digest
	configA := manifestA.Config.Digest

	// First remove B: its unique blob and base cache go, A stays intact.
	if err := engine.Create(context.Background(), "uses-b", regB.image(), nil); err != nil {
		t.Fatal(err)
	}
	engine.WaitBackground()
	idB, _ := engine.Layout.ImageID(ParseRef(regB.image()))
	baseB := filepath.Join(dir, "base", idB[len("sha256:"):])
	if _, err := os.Stat(baseB); err != nil {
		t.Fatalf("base cache was not created: %v", err)
	}
	manifestB, _ := engine.Layout.LoadManifest(ParseRef(regB.image()))
	onlyB := manifestB.Layers[1].Digest

	if err := engine.Layout.RemoveImage(ParseRef(regB.image())); err != nil {
		t.Fatal(err)
	}
	if engine.Layout.HasImage(ParseRef(regB.image())) {
		t.Fatal("removed image still registered")
	}
	if blobExists(t, dir, onlyB) {
		t.Fatal("blob unique to the removed image was kept")
	}
	if !blobExists(t, dir, shared) || !engine.Layout.HasImage(refA) {
		t.Fatal("removal damaged an image sharing the base layer")
	}
	if _, err := os.Stat(baseB); !os.IsNotExist(err) {
		t.Fatalf("base rootfs cache of the removed image survived: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "repos", regB.host(), "team", "b")); !os.IsNotExist(err) {
		t.Fatalf("empty repository directory left behind: %v", err)
	}
	if _, err := os.Stat(filepath.Join(dir, "containers", "uses-b", "ROOT", "b.txt")); err != nil {
		t.Fatalf("existing container lost its rootfs: %v", err)
	}

	// Then remove A while a legacy udocker v1 tag directory still links one of
	// its blobs by symlink only (v1 has no manifest): that blob must survive.
	legacy := filepath.Join(dir, "repos", "legacy", "img", "old")
	if err := os.MkdirAll(legacy, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink("../../../layers/"+onlyA, filepath.Join(legacy, onlyA)); err != nil {
		t.Fatal(err)
	}
	if err := engine.Layout.RemoveImage(refA); err != nil {
		t.Fatal(err)
	}
	if !blobExists(t, dir, onlyA) {
		t.Fatal("blob still linked by a legacy udocker tag was deleted")
	}
	if blobExists(t, dir, shared) || blobExists(t, dir, configA) {
		t.Fatal("unreferenced blobs of the last image were kept")
	}
	if err := engine.Layout.RemoveImage(refA); err == nil {
		t.Fatal("removing an unregistered image succeeded")
	}
}

func TestRemoveImageKeepsBaseWhileAnotherTagSharesManifest(t *testing.T) {
	engine, dir, regA, _ := twoImages(t)
	refA := ParseRef(regA.image())
	alias := ParseRef("alias.example/team/a:v1")
	if err := engine.Layout.TagImage(refA, alias); err != nil {
		t.Fatal(err)
	}
	if err := engine.Create(context.Background(), "c", regA.image(), nil); err != nil {
		t.Fatal(err)
	}
	engine.WaitBackground()
	id, _ := engine.Layout.ImageID(refA)
	base := filepath.Join(dir, "base", id[len("sha256:"):])
	if err := engine.Layout.RemoveImage(refA); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(base); err != nil {
		t.Fatalf("base cache removed while an alias still names the manifest: %v", err)
	}
	if !engine.Layout.HasImage(alias) {
		t.Fatal("alias lost its blobs")
	}
}
