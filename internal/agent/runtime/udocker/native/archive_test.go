package native

import (
	"archive/tar"
	"bytes"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

var amd64 = Platform{OS: "linux", Architecture: "amd64"}

func sha(data []byte) string {
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:])
}

// archive renders name -> content as a tar stream in the given member order
// (sorted when order is nil).
func archive(t *testing.T, members map[string][]byte, order []string) []byte {
	t.Helper()
	if order == nil {
		for name := range members {
			order = append(order, name)
		}
		sort.Strings(order)
	}
	var buf bytes.Buffer
	w := tar.NewWriter(&buf)
	dirs := map[string]bool{}
	for _, name := range order {
		if dir := filepath.Dir(name); dir != "." && !dirs[dir] {
			dirs[dir] = true
			if err := w.WriteHeader(&tar.Header{Name: dir + "/", Typeflag: tar.TypeDir, Mode: 0o755, ModTime: time.Unix(0, 0)}); err != nil {
				t.Fatal(err)
			}
		}
		data := members[name]
		if err := w.WriteHeader(&tar.Header{Name: name, Typeflag: tar.TypeReg, Mode: 0o644, Size: int64(len(data)), ModTime: time.Unix(0, 0)}); err != nil {
			t.Fatal(err)
		}
		if _, err := w.Write(data); err != nil {
			t.Fatal(err)
		}
	}
	if err := w.Close(); err != nil {
		t.Fatal(err)
	}
	return buf.Bytes()
}

// imageParts are the pieces every archive shape is built from: one plain and
// one gzip layer of the same two files, and a config naming their diff_ids.
type imageParts struct {
	plain  [][]byte // uncompressed layers
	gz     [][]byte // the same layers gzipped
	config []byte
}

func newImageParts(t *testing.T) imageParts {
	t.Helper()
	contents := []map[string]string{{"etc/os-release": "ID=test\n"}, {"app/run": "#!/bin/sh\necho ok\n"}}
	var parts imageParts
	var diffIDs []string
	for _, files := range contents {
		var entries []entry
		for name, body := range files {
			entries = append(entries, entry{name: filepath.Dir(name), dir: true}, entry{name: name, body: body})
		}
		plain := buildLayer(t, false, entries)
		parts.plain = append(parts.plain, plain)
		parts.gz = append(parts.gz, buildLayer(t, true, entries))
		diffIDs = append(diffIDs, "sha256:"+sha(plain))
	}
	config, _ := json.Marshal(map[string]any{
		"architecture": "amd64", "os": "linux",
		"config": map[string]any{"Entrypoint": []string{"/app/run"}},
		"rootfs": map[string]any{"type": "layers", "diff_ids": diffIDs},
	})
	parts.config = config
	return parts
}

// classicArchive is the docker-save layout of Docker before the containerd
// image store: <id>/layer.tar members plus legacy json/VERSION files.
func classicArchive(t *testing.T, p imageParts, tag string) []byte {
	members := map[string][]byte{sha(p.config) + ".json": p.config}
	var layers []string
	for i, layer := range p.plain {
		id := strings.Repeat(string(rune('a'+i)), 64)
		members[id+"/layer.tar"] = layer
		members[id+"/json"] = []byte(`{"id":"` + id + `"}`)
		members[id+"/VERSION"] = []byte("1.0")
		layers = append(layers, id+"/layer.tar")
	}
	manifest, _ := json.Marshal([]map[string]any{{"Config": sha(p.config) + ".json", "RepoTags": []string{tag}, "Layers": layers}})
	members["manifest.json"] = manifest
	members["repositories"] = []byte(`{}`)
	// manifest.json last, as docker writes it.
	var order []string
	for name := range members {
		if name != "manifest.json" {
			order = append(order, name)
		}
	}
	sort.Strings(order)
	return archive(t, members, append(order, "manifest.json"))
}

// ociBlobs adds the image's OCI blobs and returns the platform manifest.
func ociBlobs(p imageParts, members map[string][]byte) []byte {
	add := func(data []byte) Descriptor {
		members["blobs/sha256/"+sha(data)] = data
		return Descriptor{Digest: "sha256:" + sha(data), Size: int64(len(data))}
	}
	manifest := Manifest{SchemaVersion: 2, MediaType: mediaOCIManifest, Config: add(p.config)}
	manifest.Config.MediaType = mediaOCIConfig
	for _, layer := range p.gz {
		d := add(layer)
		d.MediaType = mediaLayerGzip
		manifest.Layers = append(manifest.Layers, d)
	}
	encoded, _ := json.Marshal(manifest)
	add(encoded)
	return encoded
}

func indexOf(entries ...map[string]any) []byte {
	data, _ := json.Marshal(map[string]any{"schemaVersion": 2, "manifests": entries})
	return data
}

func loadAndCreate(t *testing.T, data []byte, image string) (*Image, Layout) {
	t.Helper()
	layout := Layout{Dir: t.TempDir()}
	loaded, err := layout.LoadArchive(context.Background(), ParseRef(image), amd64, bytes.NewReader(data))
	if err != nil {
		t.Fatal(err)
	}
	if !layout.HasImage(ParseRef(image)) {
		t.Fatal("loaded image is not complete")
	}
	engine := &Engine{Layout: layout, Client: NewClient()}
	// Where reflink works the create schedules a base build into layout.Dir.
	t.Cleanup(engine.WaitBackground)
	if err := engine.Create(context.Background(), "c", image, nil); err != nil {
		t.Fatalf("native create from a loaded image: %v", err)
	}
	out, err := os.ReadFile(filepath.Join(layout.Dir, "containers", "c", "ROOT", "app", "run"))
	if err != nil || !strings.Contains(string(out), "echo ok") {
		t.Fatalf("container rootfs incomplete: %q %v", out, err)
	}
	return loaded, layout
}

// layerFiles lists the layer store, ignoring nothing.
func layerFiles(t *testing.T, layout Layout) []string {
	t.Helper()
	entries, err := os.ReadDir(layout.LayersDir())
	if err != nil {
		t.Fatal(err)
	}
	var names []string
	for _, entry := range entries {
		names = append(names, entry.Name())
	}
	sort.Strings(names)
	return names
}

func TestLoadClassicDockerArchive(t *testing.T) {
	p := newImageParts(t)
	image, layout := loadAndCreate(t, classicArchive(t, p, "team/app:v1"), "registry.example/team/app:v1")
	for i, layer := range image.Manifest.Layers {
		if layer.MediaType != mediaLayerTar || layer.Digest != "sha256:"+sha(p.plain[i]) {
			t.Fatalf("layer %d = %+v", i, layer)
		}
	}
	// Only config + two layers remain; json/VERSION/repositories/manifest.json are gone.
	if got := layerFiles(t, layout); len(got) != 3 {
		t.Fatalf("layer store holds unused archive files: %v", got)
	}
	config, err := layout.LoadImageConfig(ParseRef("registry.example/team/app:v1"))
	if err != nil || len(config.Entrypoint) != 1 || config.Entrypoint[0] != "/app/run" {
		t.Fatalf("config %+v %v", config, err)
	}
}

// The partition probe image the agent embeds is a `docker save | gzip`
// stream; Docker loads it as is, so the native loader has to as well. Fed to
// the tar reader unchanged it failed with "archive/tar: invalid tar header"
// and every probe on a udocker node came back failed.
func TestLoadGzippedDockerArchive(t *testing.T) {
	p := newImageParts(t)
	var packed bytes.Buffer
	zw := gzip.NewWriter(&packed)
	if _, err := zw.Write(classicArchive(t, p, "team/app:v1")); err != nil {
		t.Fatal(err)
	}
	if err := zw.Close(); err != nil {
		t.Fatal(err)
	}
	image, _ := loadAndCreate(t, packed.Bytes(), "registry.example/team/app:v1")
	if len(image.Manifest.Layers) != 2 || image.Manifest.Layers[0].Digest != "sha256:"+sha(p.plain[0]) {
		t.Fatalf("layers = %+v", image.Manifest.Layers)
	}
}

func TestLoadArchiveRejectsATruncatedGzipStream(t *testing.T) {
	var packed bytes.Buffer
	zw := gzip.NewWriter(&packed)
	_, _ = zw.Write(classicArchive(t, newImageParts(t), "team/app:v1"))
	_ = zw.Close()
	layout := Layout{Dir: t.TempDir()}
	_, err := layout.LoadArchive(context.Background(), ParseRef("team/app:v1"), amd64, bytes.NewReader(packed.Bytes()[:packed.Len()/2]))
	if err == nil {
		t.Fatal("a truncated gzip archive loaded")
	}
	if layout.HasImage(ParseRef("team/app:v1")) {
		t.Fatal("a failed load left an image behind")
	}
}

func TestLoadOCIArchiveKeepsManifestDigest(t *testing.T) {
	p := newImageParts(t)
	members := map[string][]byte{"oci-layout": []byte(`{"imageLayoutVersion":"1.0.0"}`)}
	manifest := ociBlobs(p, members)
	members["index.json"] = indexOf(map[string]any{"mediaType": mediaOCIManifest, "digest": "sha256:" + sha(manifest), "size": len(manifest)})
	// index.json first: member order must not matter.
	order := []string{"index.json", "oci-layout"}
	for name := range members {
		if strings.HasPrefix(name, "blobs/") {
			order = append(order, name)
		}
	}
	_, layout := loadAndCreate(t, archive(t, members, order), "team/app:v1")
	id, _ := layout.ImageID(ParseRef("team/app:v1"))
	if id != "sha256:"+sha(manifest) {
		t.Fatalf("image ID %s is not the archive's manifest digest", id)
	}
}

// Docker 29 with the containerd image store: index.json -> multi-platform
// index whose other platform's blobs are absent, plus a manifest.json whose
// paths point into blobs/. udocker 1.3 fails on exactly this.
func TestLoadDockerContainerdStoreArchive(t *testing.T) {
	p := newImageParts(t)
	members := map[string][]byte{"oci-layout": []byte(`{"imageLayoutVersion":"1.0.0"}`)}
	manifest := ociBlobs(p, members)
	arm := []byte(`{"schemaVersion":2,"config":{"digest":"sha256:` + strings.Repeat("0", 64) + `"},"layers":[{"digest":"sha256:` + strings.Repeat("1", 64) + `"}]}`)
	members["blobs/sha256/"+sha(arm)] = arm
	nested := indexOf(
		map[string]any{"mediaType": mediaOCIManifest, "digest": "sha256:" + sha(arm), "size": len(arm), "platform": map[string]string{"os": "linux", "architecture": "arm64"}},
		map[string]any{"mediaType": mediaOCIManifest, "digest": "sha256:" + sha(manifest), "size": len(manifest), "platform": map[string]string{"os": "linux", "architecture": "amd64"}},
	)
	members["blobs/sha256/"+sha(nested)] = nested
	members["index.json"] = indexOf(map[string]any{"mediaType": "application/vnd.oci.image.index.v1+json", "digest": "sha256:" + sha(nested), "size": len(nested)})
	var layers []string
	for _, layer := range p.gz {
		layers = append(layers, "blobs/sha256/"+sha(layer))
	}
	docker, _ := json.Marshal([]map[string]any{{"Config": "blobs/sha256/" + sha(p.config), "RepoTags": []string{"python:3.12"}, "Layers": layers}})
	members["manifest.json"] = docker

	_, layout := loadAndCreate(t, archive(t, members, nil), "python:3.12")
	id, _ := layout.ImageID(ParseRef("python:3.12"))
	if id != "sha256:"+sha(manifest) {
		t.Fatalf("image ID %s: the nested index was not followed to the amd64 manifest", id)
	}
	// Index documents and the arm64 manifest are not image content.
	if got := layerFiles(t, layout); len(got) != 3 {
		t.Fatalf("unused archive files kept: %v", got)
	}
}

func TestLoadArchiveRejectsBadArchives(t *testing.T) {
	p := newImageParts(t)
	cases := map[string]struct {
		data []byte
		ref  string
		want string
	}{}

	corrupt := map[string][]byte{"oci-layout": []byte(`{}`)}
	manifest := ociBlobs(p, corrupt)
	corrupt["index.json"] = indexOf(map[string]any{"mediaType": mediaOCIManifest, "digest": "sha256:" + sha(manifest)})
	corrupt["blobs/sha256/"+sha(p.gz[0])] = []byte("tampered")
	cases["blob content does not match its name"] = struct {
		data []byte
		ref  string
		want string
	}{archive(t, corrupt, nil), "a:b", "is corrupt"}

	bad := newImageParts(t)
	bad.plain[1] = buildLayer(t, false, []entry{{name: "evil", body: "x"}})
	cases["classic layer contradicts diff_ids"] = struct {
		data []byte
		ref  string
		want string
	}{classicArchive(t, bad, "a:b"), "a:b", "config expects"}

	two, _ := json.Marshal([]map[string]any{
		{"Config": sha(p.config) + ".json", "RepoTags": []string{"x:1"}, "Layers": []string{"l/layer.tar"}},
		{"Config": sha(p.config) + ".json", "RepoTags": []string{"y:1"}, "Layers": []string{"l/layer.tar"}},
	})
	cases["ambiguous multi-image archive"] = struct {
		data []byte
		ref  string
		want string
	}{archive(t, map[string][]byte{"manifest.json": two, sha(p.config) + ".json": p.config, "l/layer.tar": p.plain[0]}, nil), "z:1", "none is tagged"}

	armOnly := map[string][]byte{}
	armManifest := ociBlobs(p, armOnly)
	armOnly["index.json"] = indexOf(map[string]any{"mediaType": mediaOCIManifest, "digest": "sha256:" + sha(armManifest), "platform": map[string]string{"os": "linux", "architecture": "arm64"}})
	cases["no manifest for this platform"] = struct {
		data []byte
		ref  string
		want string
	}{archive(t, armOnly, nil), "a:b", "no manifest for linux/amd64"}

	cases["not an image archive"] = struct {
		data []byte
		ref  string
		want string
	}{archive(t, map[string][]byte{"README": []byte("hi")}, nil), "a:b", "neither a usable index.json nor manifest.json"}

	cases["truncated stream"] = struct {
		data []byte
		ref  string
		want string
	}{classicArchive(t, p, "a:b")[:700], "a:b", "read image archive"}

	for name, tc := range cases {
		t.Run(name, func(t *testing.T) {
			layout := Layout{Dir: t.TempDir()}
			_, err := layout.LoadArchive(context.Background(), ParseRef(tc.ref), amd64, bytes.NewReader(tc.data))
			if err == nil || !strings.Contains(err.Error(), tc.want) {
				t.Fatalf("err %v, want %q", err, tc.want)
			}
			if got := layerFiles(t, layout); len(got) != 0 {
				t.Fatalf("failed load left files in the layer store: %v", got)
			}
			if _, err := os.Stat(layout.ReposDir()); !os.IsNotExist(err) {
				t.Fatalf("failed load registered something: %v", err)
			}
		})
	}
}

func TestLoadArchiveKeepsPreexistingBlobsOnFailure(t *testing.T) {
	p := newImageParts(t)
	layout := Layout{Dir: t.TempDir()}
	if _, err := layout.LoadArchive(context.Background(), ParseRef("good:1"), amd64, bytes.NewReader(classicArchive(t, p, "good:1"))); err != nil {
		t.Fatal(err)
	}
	before := layerFiles(t, layout)
	bad := newImageParts(t)
	bad.plain[1] = buildLayer(t, false, []entry{{name: "evil", body: "x"}})
	if _, err := layout.LoadArchive(context.Background(), ParseRef("bad:1"), amd64, bytes.NewReader(classicArchive(t, bad, "bad:1"))); err == nil {
		t.Fatal("corrupt archive accepted")
	}
	if got := layerFiles(t, layout); strings.Join(got, ",") != strings.Join(before, ",") {
		t.Fatalf("a failed load removed blobs of an existing image: before %v after %v", before, got)
	}
	if !layout.HasImage(ParseRef("good:1")) {
		t.Fatal("existing image damaged")
	}
}

func TestLoadArchiveHonorsCancellation(t *testing.T) {
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	layout := Layout{Dir: t.TempDir()}
	_, err := layout.LoadArchive(ctx, ParseRef("a:b"), amd64, bytes.NewReader(classicArchive(t, newImageParts(t), "a:b")))
	if err == nil || !strings.Contains(err.Error(), "canceled") {
		t.Fatalf("cancelled load: %v", err)
	}
}

// TestLoadRealArchive times a native load of a real `docker save` archive.
// Manual: GPUWORKS_NATIVE_LOAD_ARCHIVE=/path/to.tar GPUWORKS_NATIVE_LOAD_REF=python:3.12
func TestLoadRealArchive(t *testing.T) {
	archivePath := os.Getenv("GPUWORKS_NATIVE_LOAD_ARCHIVE")
	if archivePath == "" {
		t.Skip("set GPUWORKS_NATIVE_LOAD_ARCHIVE to time a real archive")
	}
	file, err := os.Open(archivePath)
	if err != nil {
		t.Fatal(err)
	}
	defer file.Close()
	dir := os.Getenv("GPUWORKS_NATIVE_LOAD_DIR")
	if dir == "" {
		dir = t.TempDir()
	}
	engine := NewEngine(dir, nil)
	t.Cleanup(engine.WaitBackground)
	image := os.Getenv("GPUWORKS_NATIVE_LOAD_REF")
	started := time.Now()
	if err := engine.Load(context.Background(), image, io.Reader(file)); err != nil {
		t.Fatal(err)
	}
	loaded := time.Since(started)
	started = time.Now()
	if err := engine.Create(context.Background(), "bench", image, nil); err != nil {
		t.Fatal(err)
	}
	created := time.Since(started)
	started = time.Now()
	if err := engine.Create(context.Background(), "bench-2", image, nil); err != nil {
		t.Fatal(err)
	}
	t.Logf("native load_s=%.1f create_s=%.1f second_create_s=%.1f", loaded.Seconds(), created.Seconds(), time.Since(started).Seconds())
}
