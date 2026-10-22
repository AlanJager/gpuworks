package native

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/klauspost/compress/zstd"
)

func zstdCompress(t *testing.T, data []byte) []byte {
	t.Helper()
	var buf bytes.Buffer
	enc, err := zstd.NewWriter(&buf)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := enc.Write(data); err != nil {
		t.Fatal(err)
	}
	if err := enc.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// withLayerMedia rewrites the served manifest's layer media types, the way a
// registry advertises zstd layers.
func (r *fakeRegistry) withLayerMedia(t *testing.T, media ...string) {
	t.Helper()
	var manifest Manifest
	if err := json.Unmarshal(r.manifest, &manifest); err != nil {
		t.Fatal(err)
	}
	for i, m := range media {
		if m != "" {
			manifest.Layers[i].MediaType = m
		}
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	r.manifest = encoded
}

func TestEnginePullsAndUnpacksZstdLayers(t *testing.T) {
	base := buildLayer(t, true, []entry{{name: "etc", dir: true}, {name: "etc/os-release", body: "ID=test\n"}})
	plain := buildLayer(t, false, []entry{{name: "app", dir: true}, {name: "app/model.bin", body: "weights", pad: 2 << 20}})
	zst := zstdCompress(t, plain)
	reg := newFakeRegistry(t, "team/zstd", [][]byte{base, zst}, []byte(`{"config":{}}`))
	reg.withLayerMedia(t, "", "application/vnd.oci.image.layer.v1.tar+zstd")
	// A sidecar threshold below the zstd layer's size: the plan must still
	// leave it alone, because sidecars only inflate gzip.
	engine, dir := engineFor(t, reg, 1)

	if err := engine.Pull(context.Background(), reg.image(), nil); err != nil {
		t.Fatalf("pull with a zstd layer: %v", err)
	}
	sum := sha256.Sum256(zst)
	if _, err := os.Stat(filepath.Join(dir, "tarcache", "sha256-"+hex.EncodeToString(sum[:])+".tar")); !os.IsNotExist(err) {
		t.Fatalf("a gzip sidecar was attempted for the zstd layer: %v", err)
	}
	if err := engine.Create(context.Background(), "c", reg.image(), nil); err != nil {
		t.Fatalf("create from zstd layer: %v", err)
	}
	data, err := os.ReadFile(filepath.Join(dir, "containers", "c", "ROOT", "app", "model.bin"))
	if err != nil || !strings.HasPrefix(string(data), "weights") || len(data) != len("weights")+2<<20 {
		t.Fatalf("zstd layer content wrong: len=%d err=%v", len(data), err)
	}
	if _, err := os.Stat(filepath.Join(dir, "containers", "c", "ROOT", "etc", "os-release")); err != nil {
		t.Fatal(err)
	}
}

func TestEngineRejectsUnpackableLayersBeforeDownloading(t *testing.T) {
	for _, media := range []string{
		"application/vnd.oci.image.layer.v1.tar+zstd+encrypted",
		"application/vnd.docker.image.rootfs.foreign.diff.tar.gzip",
	} {
		reg := newFakeRegistry(t, "team/x", [][]byte{buildLayer(t, true, []entry{{name: "a", body: "a"}})}, []byte(`{"config":{}}`))
		reg.withLayerMedia(t, media)
		engine, dir := engineFor(t, reg, 0)
		if err := engine.Pull(context.Background(), reg.image(), nil); err == nil || !strings.Contains(err.Error(), "not supported") {
			t.Fatalf("%s: %v", media, err)
		}
		if entries, _ := os.ReadDir(filepath.Join(dir, "layers")); len(entries) != 0 {
			t.Fatalf("%s: blobs downloaded before rejection: %v", media, entries)
		}
	}
}
