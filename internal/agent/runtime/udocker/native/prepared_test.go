package native

import (
	"context"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
)

// gatedRegistry serves a two-layer image whose second (large) blob is held
// back until release is closed, or failed with 500 when failBig is set.
func gatedRegistry(t *testing.T, failBig bool) (*fakeRegistry, chan struct{}) {
	t.Helper()
	small := buildLayer(t, true, []entry{{name: "etc", dir: true}, {name: "etc/small", body: "early"}})
	big := buildLayer(t, true, []entry{{name: "opt", dir: true}, {name: "opt/big.bin", body: "late", pad: 3 << 20}})
	reg := newFakeRegistry(t, "team/prep", [][]byte{small, big}, []byte(`{"config":{}}`))
	bigDigest := reg.layerDigests(t)[1]
	release := make(chan struct{})
	inner := reg.server.Config.Handler
	reg.server.Config.Handler = http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if strings.HasSuffix(r.URL.Path, bigDigest) {
			if failBig {
				w.WriteHeader(http.StatusInternalServerError)
				return
			}
			<-release
		}
		inner.ServeHTTP(w, r)
	})
	return reg, release
}

func launchPull(engine *Engine, image string) chan error {
	done := make(chan error, 1)
	go func() { done <- engine.Pull(runtime.WithLaunchIntent(context.Background()), image, nil) }()
	return done
}

func TestLaunchPullUnpacksLayersWhileTheImageDownloads(t *testing.T) {
	reg, release := gatedRegistry(t, false)
	engine, dir := engineFor(t, reg, 0)
	pulled := launchPull(engine, reg.image())

	// The small layer is extracted while the big one is still held back.
	early := filepath.Join(dir, "prepared")
	deadline := time.Now().Add(10 * time.Second)
	for {
		matches, _ := filepath.Glob(filepath.Join(early, "*.building", "ROOT", "etc", "small"))
		if len(matches) == 1 {
			break
		}
		if time.Now().After(deadline) {
			close(release)
			t.Fatal("no layer was unpacked before the download finished")
		}
		time.Sleep(20 * time.Millisecond)
	}
	close(release)
	if err := <-pulled; err != nil {
		t.Fatal(err)
	}

	lines := createLines(t, engine, "c1", reg.image())
	if !mentions(lines, "used the root filesystem unpacked during the pull") || mentions(lines, "Extracting") {
		t.Fatalf("create did not claim the prepared rootfs: %v", lines)
	}
	for rel, want := range map[string]string{"etc/small": "early", "opt/big.bin": "late"} {
		data, err := os.ReadFile(filepath.Join(dir, "containers", "c1", "ROOT", rel))
		if err != nil || !strings.HasPrefix(string(data), want) {
			t.Fatalf("%s: %q %v", rel, data[:min(len(data), 8)], err)
		}
	}
	if entries, _ := os.ReadDir(early); len(entries) != 0 {
		t.Fatalf("prepared tree left after being claimed: %v", entries)
	}
	// A prepared tree is claimed once; the next container unpacks.
	if second := createLines(t, engine, "c2", reg.image()); mentions(second, "unpacked during the pull") {
		t.Fatalf("prepared tree claimed twice: %v", second)
	}
}

func TestPullWithoutLaunchIntentPreparesNothing(t *testing.T) {
	reg, release := gatedRegistry(t, false)
	close(release)
	engine, dir := engineFor(t, reg, 0)
	if err := engine.Pull(context.Background(), reg.image(), nil); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(dir, "prepared")); !os.IsNotExist(err) {
		t.Fatalf("a distribution pull prepared a rootfs: %v", err)
	}
}

func TestFailedLaunchPullLeavesNoPreparedRootfs(t *testing.T) {
	reg, _ := gatedRegistry(t, true)
	engine, dir := engineFor(t, reg, 0)
	if err := <-launchPull(engine, reg.image()); err == nil {
		t.Fatal("pull of an image whose layer fails succeeded")
	}
	id := digestOf(reg.manifest)
	engine.prepared.mu.Lock()
	job := engine.prepared.jobs[id]
	engine.prepared.mu.Unlock()
	if job == nil {
		t.Fatal("no prepare job was started")
	}
	<-job.done
	if job.err == nil {
		t.Fatal("prepare job succeeded without its layer")
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "prepared")); len(entries) != 0 {
		t.Fatalf("failed pull left a prepared rootfs: %v", entries)
	}
}

func TestRemoveImageDropsAnUnclaimedPreparedRootfs(t *testing.T) {
	reg, release := gatedRegistry(t, false)
	close(release)
	engine, dir := engineFor(t, reg, 0)
	if err := <-launchPull(engine, reg.image()); err != nil {
		t.Fatal(err)
	}
	id := digestOf(reg.manifest)
	engine.prepared.mu.Lock()
	job := engine.prepared.jobs[id]
	engine.prepared.mu.Unlock()
	<-job.done
	if !isCompleteBase(filepath.Join(engine.Layout.preparedDir(id), "ROOT")) {
		t.Fatal("prepared rootfs was not published")
	}
	if err := engine.Layout.RemoveImage(ParseRef(reg.image())); err != nil {
		t.Fatal(err)
	}
	if entries, _ := os.ReadDir(filepath.Join(dir, "prepared")); len(entries) != 0 {
		t.Fatalf("prepared rootfs outlived its image: %v", entries)
	}
}
