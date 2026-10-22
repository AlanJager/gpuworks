package udocker

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/agent/imagepull"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

const (
	cliPullLayerA = "sha256:1111111111111111111111111111111111111111111111111111111111111111"
	cliPullLayerB = "sha256:2222222222222222222222222222222222222222222222222222222222222222"
)

// cliPullImage is pulled from a registry other than Docker Hub, as the
// platform's images are.
const cliPullImage = "registry.example/team/image:tag"

// cliPullDriver is a driver whose udocker CLI is a script: `pull` runs
// pullScript with $LAYERS set to the layer store and the image's manifest
// already written where udocker 1.3 writes it (repos/<reference with its
// host>/<tag>), before it fetches the layers — as seen on a real pull.
func cliPullDriver(t *testing.T, pullScript string) *Driver {
	t.Helper()
	t.Setenv(launcherEnv, "udocker")
	t.Setenv(disableNativeEnv, "1")
	base := t.TempDir()
	makeUdockerTempRootTraversable(t, base)
	bundle := filepath.Join(base, "bundle")
	if err := os.MkdirAll(filepath.Join(bundle, "bin"), 0o755); err != nil {
		t.Fatal(err)
	}
	d := New(base)
	d.bundleDir = bundle
	d.embeddedBundle = false
	d.runUser = ""
	manifestDir := filepath.Join(d.udockerDir, "repos", "registry.example", "team", "image", "tag")
	manifest := `{"layers":[{"digest":"` + cliPullLayerA + `","size":100},{"digest":"` + cliPullLayerB + `","size":300}]}`
	pull := "mkdir -p '" + manifestDir + "' '" + filepath.Join(d.udockerDir, "layers") + "'\n" +
		"printf '%s' '" + manifest + "' > '" + filepath.Join(manifestDir, "manifest") + "'\n" +
		"LAYERS='" + filepath.Join(d.udockerDir, "layers") + "'\n" + pullScript
	script := "#!/bin/sh\ncase \"$1 $2\" in\n" +
		"\"--allow-root --version\") echo udocker 1.3.17;;\n" +
		"\"--allow-root pull\")\n" + pull + "\n;;\n" +
		"*) echo ok;;\nesac\n"
	if err := os.WriteFile(filepath.Join(bundle, "bin", "udocker"), []byte(script), 0o755); err != nil {
		t.Fatal(err)
	}
	return d
}

// writeBytes is a shell fragment that makes a layer file n bytes long.
func writeBytes(digest string, n int) string {
	return "head -c " + strconv.Itoa(n) + " /dev/zero > \"$LAYERS/" + digest + "\"\n"
}

// The udocker CLI prints no byte counts, so the driver reads them off the
// layer store, layer by layer, in the shared pull lines: each count is tied to
// its layer and a later one finishes it. Unchanged layers are not repeated.
func TestCLIPullReportsEachLayerOfItsStore(t *testing.T) {
	d := cliPullDriver(t, writeBytes(cliPullLayerA, 100)+writeBytes(cliPullLayerB, 120)+
		"sleep 1.4\n"+writeBytes(cliPullLayerB, 300)+"sleep 1.4\necho pulled\n")
	var lines []string
	if err := d.PullImage(context.Background(), cliPullImage, func(line string) { lines = append(lines, line) }); err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(lines, "\n")
	for _, want := range []string{
		runtime.PullLineDownloading(cliPullLayerA, 100, 100),
		runtime.PullLineDownloading(cliPullLayerB, 120, 300),
		runtime.PullLineDownloading(cliPullLayerB, 300, 300),
	} {
		if !strings.Contains(joined, want) {
			t.Fatalf("missing %q in:\n%s", want, joined)
		}
	}
	if n := strings.Count(joined, runtime.PullLineDownloading(cliPullLayerA, 100, 100)); n != 1 {
		t.Fatalf("an unchanged layer was reported %d times:\n%s", n, joined)
	}
	if strings.Contains(joined, "udocker storage") {
		t.Fatalf("the store's growth was reported without layers although the manifest was there:\n%s", joined)
	}
}

// A layer file that stops growing is a stalled download: the node's stall
// watch gives the source up instead of waiting out the candidate timeout.
func TestWalkGivesUpOnAnUdockerCLIPullThatStops(t *testing.T) {
	oldStall, oldBeat := imagepull.StallTimeout, imagepull.HeartbeatInterval
	// The store is read once a second, so the window must span a few reads.
	imagepull.StallTimeout, imagepull.HeartbeatInterval = 2500*time.Millisecond, 50*time.Millisecond
	t.Cleanup(func() { imagepull.StallTimeout, imagepull.HeartbeatInterval = oldStall, oldBeat })
	// The pull then just waits. As root the CLI runs under runuser and bash,
	// which the context's kill does not reach, so the wait is kept short
	// enough to end on its own; the stall must be judged well before it.
	d := cliPullDriver(t, writeBytes(cliPullLayerA, 100)+writeBytes(cliPullLayerB, 120)+"sleep 8\n")
	started := time.Now()
	_, err := imagepull.New(imagepull.Config{}).Walk(context.Background(), d,
		[]proto.ImagePullCandidate{{Ref: cliPullImage}}, imagepull.Options{}, imagepull.Hooks{})
	var all *imagepull.AllFailedError
	if !errors.As(err, &all) || !strings.Contains(err.Error(), "no download progress") {
		t.Fatalf("a stopped udocker CLI pull was not given up on: %v", err)
	}
	if elapsed := time.Since(started); elapsed > 30*time.Second {
		t.Fatalf("gave up after %s, want soon after the stall window", elapsed)
	}
}
