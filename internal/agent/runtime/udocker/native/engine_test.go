package native

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math/rand"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"
	"time"
)

// fakeRegistry serves a single image over the parts of the v2 API this package
// uses, with range support so the ranged download path is exercised too.
type fakeRegistry struct {
	server   *httptest.Server
	blobs    map[string][]byte
	manifest []byte
	repo     string
}

func newFakeRegistry(t *testing.T, repo string, layers [][]byte, config []byte) *fakeRegistry {
	t.Helper()
	reg := &fakeRegistry{blobs: map[string][]byte{}, repo: repo}
	descriptor := func(data []byte, mediaType string) Descriptor {
		sum := sha256.Sum256(data)
		digest := "sha256:" + hex.EncodeToString(sum[:])
		reg.blobs[digest] = data
		return Descriptor{MediaType: mediaType, Digest: digest, Size: int64(len(data))}
	}
	manifest := Manifest{
		SchemaVersion: 2,
		MediaType:     "application/vnd.docker.distribution.manifest.v2+json",
		Config:        descriptor(config, "application/vnd.docker.container.image.v1+json"),
	}
	for _, layer := range layers {
		manifest.Layers = append(manifest.Layers, descriptor(layer, "application/vnd.docker.image.rootfs.diff.tar.gzip"))
	}
	encoded, err := json.Marshal(manifest)
	if err != nil {
		t.Fatal(err)
	}
	reg.manifest = encoded

	mux := http.NewServeMux()
	mux.HandleFunc("/v2/", func(w http.ResponseWriter, r *http.Request) {
		// Handlers run concurrently: a single blob is fetched by several range
		// requests at once. Everything touched here is read-only after setup.
		path := strings.TrimPrefix(r.URL.Path, "/v2/")
		switch {
		case path == "":
			w.WriteHeader(http.StatusOK)
		case strings.HasPrefix(path, repo+"/manifests/"):
			w.Header().Set("Content-Type", manifest.MediaType)
			w.Write(reg.manifest)
		case strings.HasPrefix(path, repo+"/blobs/"):
			digest := strings.TrimPrefix(path, repo+"/blobs/")
			data, ok := reg.blobs[digest]
			if !ok {
				w.WriteHeader(http.StatusNotFound)
				return
			}
			// http.ServeContent implements Range for us.
			w.Header().Set("Content-Type", "application/octet-stream")
			http.ServeContent(w, r, "", time.Time{}, bytes.NewReader(data))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
	reg.server = httptest.NewTLSServer(mux)
	t.Cleanup(reg.server.Close)
	return reg
}

func (r *fakeRegistry) host() string {
	return strings.TrimPrefix(r.server.URL, "https://")
}

// image is the reference as a user would write it, including the host.
func (r *fakeRegistry) image() string {
	return r.host() + "/" + r.repo + ":latest"
}

// layerDigests returns the served manifest's layer digests in order.
func (r *fakeRegistry) layerDigests(t *testing.T) []string {
	t.Helper()
	var manifest Manifest
	if err := json.Unmarshal(r.manifest, &manifest); err != nil {
		t.Fatal(err)
	}
	digests := make([]string, 0, len(manifest.Layers))
	for _, layer := range manifest.Layers {
		digests = append(digests, layer.Digest)
	}
	return digests
}

// engineFor builds an Engine pointed at a temp UDOCKER_DIR and the fake
// registry, over plain HTTP.
func engineFor(t *testing.T, reg *fakeRegistry, sidecarMin int64) (*Engine, string) {
	t.Helper()
	dir := t.TempDir()
	engine := &Engine{
		Layout:     Layout{Dir: dir},
		Client:     NewClient(),
		Workers:    4,
		RangeParts: 4,
		RangeMin:   1 << 20,
	}
	t.Cleanup(engine.WaitBackground)
	// Borrow the test server's client so its self-signed certificate is
	// trusted; the URL scheme the client builds stays untouched.
	engine.Client.HTTP = reg.server.Client()
	if sidecarMin > 0 {
		engine.Sidecars = &sidecarStore{
			Dir:     filepath.Join(dir, "tarcache"),
			MinSize: sidecarMin,
			Reserve: 0,
		}
	}
	return engine, dir
}

func treeOf(t *testing.T, root string) []string {
	t.Helper()
	var lines []string
	err := filepath.Walk(root, func(path string, info os.FileInfo, err error) error {
		if err != nil {
			return err
		}
		rel, _ := filepath.Rel(root, path)
		if rel == "." {
			return nil
		}
		switch {
		case info.IsDir():
			lines = append(lines, fmt.Sprintf("d %s %o", rel, info.Mode().Perm()))
		case info.Mode()&os.ModeSymlink != 0:
			target, _ := os.Readlink(path)
			lines = append(lines, fmt.Sprintf("l %s -> %s", rel, target))
		default:
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			sum := sha256.Sum256(data)
			lines = append(lines, fmt.Sprintf("f %s %o %s", rel, info.Mode().Perm(), hex.EncodeToString(sum[:8])))
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	sort.Strings(lines)
	return lines
}

// The whole point of the sidecar is that it changes only timing. A pull that
// pre-inflates large layers and one that does not must produce byte-identical
// container filesystems.
func TestEngineSidecarProducesIdenticalRootfs(t *testing.T) {
	base := buildLayer(t, true, []entry{
		{name: "etc", dir: true},
		{name: "etc/base.conf", body: "base"},
		{name: "usr/lib/libbase.so", body: strings.Repeat("base-payload", 4096)},
		{name: "usr/lib/alias", symlink: "libbase.so"},
		{name: "etc/stale", body: "stale"},
	})
	top := buildLayer(t, true, []entry{
		{name: "etc/.wh.stale"},
		{name: "etc/base.conf", body: "overridden"},
		{name: "usr/lib/libtop.so", body: strings.Repeat("top-payload", 4096)},
	})
	config := []byte(`{"architecture":"amd64","os":"linux","config":{"Entrypoint":["/bin/sh"]}}`)
	reg := newFakeRegistry(t, "team/img", [][]byte{base, top}, config)

	var trees [][]string
	for _, name := range []string{"plain", "sidecar"} {
		// A sidecar minimum of 1 byte forces every layer through the streaming
		// path; 0 disables it entirely.
		var min int64
		if name == "sidecar" {
			min = 1
		}
		engine, dir := engineFor(t, reg, min)
		image := reg.image()
		if err := engine.Pull(context.Background(), image, nil); err != nil {
			t.Fatalf("%s: Pull: %v", name, err)
		}
		if name == "sidecar" {
			entries, err := os.ReadDir(filepath.Join(dir, "tarcache"))
			if err != nil || len(entries) == 0 {
				t.Fatalf("sidecar run produced no cached tars (err=%v)", err)
			}
		}
		if err := engine.Create(context.Background(), "c1", image, nil); err != nil {
			t.Fatalf("%s: Create: %v", name, err)
		}
		trees = append(trees, treeOf(t, filepath.Join(dir, "containers", "c1", "ROOT")))
	}
	if strings.Join(trees[0], "\n") != strings.Join(trees[1], "\n") {
		t.Errorf("sidecar rootfs differs from the plain one:\nplain:\n%s\nsidecar:\n%s",
			strings.Join(trees[0], "\n"), strings.Join(trees[1], "\n"))
	}
	// Sanity: the whiteout and override actually took effect in both.
	if len(trees[0]) == 0 {
		t.Fatal("empty rootfs")
	}
	for _, line := range trees[0] {
		if strings.HasPrefix(line, "f etc/stale") {
			t.Error("whiteout did not remove etc/stale")
		}
	}
}

func TestEngineCreateReusesDigestBase(t *testing.T) {
	layer := buildLayer(t, true, []entry{{name: "etc/model", body: "ok"}})
	config := []byte(`{"architecture":"amd64","os":"linux"}`)
	reg := newFakeRegistry(t, "team/base", [][]byte{layer}, config)
	engine, dir := engineFor(t, reg, 0)
	engine.Reflink = func(string) bool { return true }
	t.Cleanup(engine.WaitBackground)
	image := reg.image()
	if err := engine.Pull(context.Background(), image, nil); err != nil {
		t.Fatal(err)
	}
	if err := engine.Create(context.Background(), "c1", image, nil); err != nil {
		t.Fatal(err)
	}
	engine.WaitBackground()
	var lines []string
	if err := engine.Create(context.Background(), "c2", image, func(line string) { lines = append(lines, line) }); err != nil {
		t.Fatal(err)
	}
	found := false
	for _, line := range lines {
		if strings.Contains(line, "reused immutable base rootfs") {
			found = true
		}
	}
	if !found {
		t.Fatalf("create progress=%v", lines)
	}
	if _, err := os.Stat(filepath.Join(dir, "base")); err != nil {
		t.Fatalf("base cache missing: %v", err)
	}
}

// A sidecar derived from bytes that fail verification must never be published.
func TestEngineSidecarDiscardedWhenBlobCorrupt(t *testing.T) {
	layer := buildLayer(t, true, []entry{{name: "etc/conf", body: strings.Repeat("x", 8192)}})
	config := []byte(`{"architecture":"amd64","os":"linux"}`)
	reg := newFakeRegistry(t, "team/bad", [][]byte{layer}, config)
	decoy := buildLayer(t, true, []entry{{name: "etc/conf", body: strings.Repeat("y", 8192)}})
	reg.blobs[reg.layerDigests(t)[0]] = decoy

	engine, dir := engineFor(t, reg, 1)
	err := engine.Pull(context.Background(), reg.image(), nil)
	if err == nil {
		t.Fatal("Pull accepted a blob whose digest does not match")
	}
	entries, _ := os.ReadDir(filepath.Join(dir, "tarcache"))
	for _, entry := range entries {
		if !strings.HasPrefix(entry.Name(), ".tar-") {
			t.Errorf("sidecar %s was published for an unverified blob", entry.Name())
		}
	}
}

// A sidecar whose layer is gone can never be used again and must be reclaimed.
func TestSidecarSweepRemovesOrphansAndPartials(t *testing.T) {
	dir := t.TempDir()
	layers := filepath.Join(dir, "layers")
	cache := filepath.Join(dir, "tarcache")
	for _, d := range []string{layers, cache} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	live := "sha256:" + strings.Repeat("a", 64)
	orphan := "sha256:" + strings.Repeat("b", 64)
	if err := os.WriteFile(filepath.Join(layers, live), []byte("x"), 0o644); err != nil {
		t.Fatal(err)
	}
	store := &sidecarStore{Dir: cache}
	for _, name := range []string{
		strings.ReplaceAll(live, ":", "-") + ".tar",
		strings.ReplaceAll(orphan, ":", "-") + ".tar",
		".tar-partial123",
	} {
		if err := os.WriteFile(filepath.Join(cache, name), []byte("data"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	store.sweep(layers)

	if _, err := os.Stat(filepath.Join(cache, strings.ReplaceAll(live, ":", "-")+".tar")); err != nil {
		t.Errorf("sweep removed the sidecar of a live layer: %v", err)
	}
	if _, err := os.Stat(filepath.Join(cache, strings.ReplaceAll(orphan, ":", "-")+".tar")); !os.IsNotExist(err) {
		t.Error("sweep kept a sidecar whose layer is gone")
	}
	if _, err := os.Stat(filepath.Join(cache, ".tar-partial123")); !os.IsNotExist(err) {
		t.Error("sweep kept a partial sidecar from an interrupted pull")
	}
}

func TestDigestFromSidecarRoundTrips(t *testing.T) {
	store := &sidecarStore{Dir: "/tmp/cache"}
	digest := "sha256:" + strings.Repeat("c", 64)
	name := filepath.Base(store.path(digest))
	got, ok := digestFromSidecar(name)
	if !ok || got != digest {
		t.Errorf("digestFromSidecar(%q) = %q, %v; want %q", name, got, ok, digest)
	}
	for _, bad := range []string{"notours.txt", "sha256-.tar", "-abc.tar", "plain"} {
		if _, ok := digestFromSidecar(bad); ok {
			t.Errorf("digestFromSidecar(%q) claimed an unrelated file", bad)
		}
	}
}

// A large layer is fetched by several concurrent range requests, so its bytes
// land out of order while the sidecar is inflating them front to back. That
// interaction is the whole risk of streaming during a download, so exercise it
// end to end with a layer big enough to be split.
func TestEngineSidecarWithRangedDownload(t *testing.T) {
	// The layer has to exceed engineFor's 1 MiB RangeMin *after* compression,
	// so the payload must be incompressible; repeated text would deflate to
	// almost nothing and the download would never be split.
	rng := rand.New(rand.NewSource(7))
	var entries []entry
	for i := 0; i < 40; i++ {
		body := make([]byte, 64<<10)
		if _, err := rng.Read(body); err != nil {
			t.Fatal(err)
		}
		entries = append(entries, entry{
			name: fmt.Sprintf("usr/lib/pkg%d/data.bin", i),
			body: string(body),
		})
	}
	entries = append(entries, entry{name: "usr/lib/link", symlink: "pkg0/data.bin"})
	layer := buildLayer(t, true, entries)
	config := []byte(`{"architecture":"amd64","os":"linux"}`)
	reg := newFakeRegistry(t, "team/big", [][]byte{layer}, config)

	if int64(len(layer)) <= 1<<20 {
		t.Fatalf("layer is %d bytes, too small to be split into ranges", len(layer))
	}

	var trees [][]string
	for _, name := range []string{"plain", "sidecar"} {
		var min int64
		if name == "sidecar" {
			min = 1
		}
		engine, dir := engineFor(t, reg, min)
		if err := engine.Pull(context.Background(), reg.image(), nil); err != nil {
			t.Fatalf("%s: Pull: %v", name, err)
		}
		if name == "sidecar" {
			// The sidecar must hold the layer inflated in full, which is only
			// true if the prefix reader followed the out-of-order writes
			// correctly through to the gzip trailer.
			path, ok := engine.Sidecars.Get(Descriptor{Digest: reg.layerDigests(t)[0]})
			if !ok {
				t.Fatal("no sidecar produced for a ranged download")
			}
			info, err := os.Stat(path)
			if err != nil {
				t.Fatal(err)
			}
			if info.Size() <= int64(len(layer)) {
				t.Errorf("sidecar is %d bytes, not larger than the %d-byte compressed layer", info.Size(), len(layer))
			}
		}
		if err := engine.Create(context.Background(), "c1", reg.image(), nil); err != nil {
			t.Fatalf("%s: Create: %v", name, err)
		}
		trees = append(trees, treeOf(t, filepath.Join(dir, "containers", "c1", "ROOT")))
	}
	if strings.Join(trees[0], "\n") != strings.Join(trees[1], "\n") {
		t.Error("ranged sidecar rootfs differs from the plain one")
	}
	if len(trees[0]) < 40 {
		t.Fatalf("rootfs has %d entries, want at least 40", len(trees[0]))
	}
}

func TestSidecarPlanRespectsCumulativeBudget(t *testing.T) {
	dir := t.TempDir()
	free := availableAt(dir)
	if free <= 0 {
		t.Skip("cannot determine free space on this filesystem")
	}
	const gib = 1 << 30
	layer := func(tag string, size int64) Descriptor {
		return Descriptor{Digest: "sha256:" + strings.Repeat(tag, 64), Size: size}
	}

	// Leave room for exactly one 1 GiB layer: each costs size*inflatedSizeFactor.
	store := &sidecarStore{
		Dir:     dir,
		MinSize: 512 << 20,
		Reserve: free - (gib*inflatedSizeFactor + gib/2),
	}
	planned := store.Plan([]Descriptor{layer("a", gib), layer("b", gib), layer("c", gib)})
	if len(planned) != 1 {
		t.Fatalf("planned %d layers, want 1: a per-layer check would admit all three", len(planned))
	}

	// Small layers are not worth the disk regardless of budget.
	planned = store.Plan([]Descriptor{layer("d", 1<<20), layer("e", 700<<20)})
	if len(planned) != 1 || planned[0].Size != 700<<20 {
		t.Errorf("planned %v, want only the 700 MiB layer", planned)
	}

	// Largest first, so a partial budget buys the most time. Set the budget
	// explicitly instead of assuming this filesystem has 9 GiB available.
	store.Reserve = free - (3*gib*inflatedSizeFactor + gib/2)
	planned = store.Plan([]Descriptor{layer("f", 600<<20), layer("g", 2*gib), layer("h", gib)})
	if len(planned) != 2 || planned[0].Digest != layer("g", 2*gib).Digest || planned[1].Digest != layer("h", gib).Digest {
		t.Errorf("planned %v, want the 2 GiB layer followed by the 1 GiB layer", planned)
	}

	// A layer already inflated by an earlier pull is not redone.
	done := layer("i", gib)
	if err := os.WriteFile(store.path(done.Digest), []byte("cached"), 0o644); err != nil {
		t.Fatal(err)
	}
	if planned := store.Plan([]Descriptor{done}); len(planned) != 0 {
		t.Errorf("planned %v, want nothing: a sidecar already exists", planned)
	}

	// An unknowable or exhausted filesystem plans nothing rather than guessing.
	store.Reserve = free + gib
	if planned := store.Plan([]Descriptor{layer("j", gib)}); len(planned) != 0 {
		t.Errorf("planned %v with no room left", planned)
	}
	if planned := (&sidecarStore{Dir: ""}).Plan([]Descriptor{layer("k", gib)}); planned != nil {
		t.Error("a disabled store planned work")
	}
}

func TestSidecarEvictsLeastRecentlyUsedOverCap(t *testing.T) {
	dir := t.TempDir()
	layers := filepath.Join(dir, "layers")
	cache := filepath.Join(dir, "tarcache")
	for _, d := range []string{layers, cache} {
		if err := os.MkdirAll(d, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	// Three sidecars of 1000 bytes each, all with live layers, under a cap
	// that only fits two.
	store := &sidecarStore{Dir: cache, MaxTotal: 2500}
	digests := make([]string, 3)
	base := time.Now().Add(-time.Hour)
	for i := range digests {
		digests[i] = fmt.Sprintf("sha256:%064d", i)
		if err := os.WriteFile(filepath.Join(layers, digests[i]), []byte("layer"), 0o644); err != nil {
			t.Fatal(err)
		}
		path := store.path(digests[i])
		if err := os.WriteFile(path, make([]byte, 1000), 0o644); err != nil {
			t.Fatal(err)
		}
		// Ascending age: index 0 is the oldest.
		stamp := base.Add(time.Duration(i) * time.Minute)
		if err := os.Chtimes(path, stamp, stamp); err != nil {
			t.Fatal(err)
		}
	}

	// Reading the oldest marks it as used, so eviction must spare it and take
	// the next-oldest instead.
	if _, ok := store.Get(Descriptor{Digest: digests[0]}); !ok {
		t.Fatal("Get did not find a sidecar that exists")
	}
	store.sweep(layers)

	if _, err := os.Stat(store.path(digests[0])); err != nil {
		t.Error("evicted the sidecar that was just used")
	}
	if _, err := os.Stat(store.path(digests[1])); !os.IsNotExist(err) {
		t.Error("kept the least-recently-used sidecar over the cap")
	}
	if _, err := os.Stat(store.path(digests[2])); err != nil {
		t.Error("evicted more than needed to reach the cap")
	}

	// A zero cap means unbounded; nothing is reclaimed for size alone.
	unbounded := &sidecarStore{Dir: cache}
	before, _ := os.ReadDir(cache)
	unbounded.sweep(layers)
	after, _ := os.ReadDir(cache)
	if len(after) != len(before) {
		t.Errorf("an uncapped cache evicted %d entries", len(before)-len(after))
	}
}
