//go:build docker

package l3

import (
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	dockerdrv "gpuworks.ai/gpuworks/internal/agent/runtime/docker"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// l3RegistryImage is the distribution registry the case runs locally. The
// quay copy is used because Docker Hub is not reachable from every runner.
const l3RegistryImage = "quay.io/libpod/registry:2.8.2"

// TestRollingTagForcedRepullPicksUpNewContent is l3-rolling-tag-repull-local-
// registry: a nightly tag is pushed to a local registry, pulled onto a docker
// node, and then pushed again with different content. A plain sync leaves the
// node on the copy it has; a forced one pulls the new content, and the node
// row's digest and update time move with it.
//
// The node's docker daemon is also the one the test pushes from, so every
// push goes through a different spelling of the same registry (127.0.0.1
// instead of localhost): the node's own tag keeps pointing at the first build
// until the forced pull replaces it.
func TestRollingTagForcedRepullPicksUpNewContent(t *testing.T) {
	if _, err := exec.LookPath("docker"); err != nil {
		skipDocker(t, err)
	}
	if err := exec.Command("docker", "info").Run(); err != nil {
		skipDocker(t, err)
	}
	_, file, _, _ := runtime.Caller(0)
	root := filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))
	if err := ensureFakeImage(t, root); err != nil {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	if exec.Command("docker", "image", "inspect", l3RegistryImage).Run() != nil {
		if out, err := exec.Command("docker", "pull", l3RegistryImage).CombinedOutput(); err != nil {
			if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
				t.Fatalf("pull %s: %v: %s", l3RegistryImage, err, out)
			}
			t.Skipf("registry image unavailable: %v: %s", err, out)
		}
	}

	// The local registry needs no credentials. An empty client config keeps
	// docker (here and in the node's runtime driver, which inherits the
	// environment) from consulting a desktop credential helper, which can
	// hang without a logged-in session.
	dockerConfig := t.TempDir()
	if err := os.WriteFile(filepath.Join(dockerConfig, "config.json"), []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("DOCKER_CONFIG", dockerConfig)

	// The registry shares the daemon's network namespace, so the daemon
	// reaches it on its own loopback whether it runs on the host or in a VM
	// (Docker Desktop), where a port published on the host is out of reach.
	port := strconv.Itoa(reservePort(t))
	registry := fmt.Sprintf("gpuworks-l3-registry-%d", time.Now().UnixNano())
	if out, err := exec.Command("docker", "run", "-d", "--name", registry, "--network", "host", "-e", "REGISTRY_HTTP_ADDR=127.0.0.1:"+port, l3RegistryImage).CombinedOutput(); err != nil {
		t.Fatalf("start registry: %v: %s", err, out)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", registry).Run() })
	waitForRegistry(t, registry, port)

	ref := "localhost:" + port + "/l3/rolling:nightly"
	alias := "127.0.0.1:" + port + "/l3/rolling:nightly"
	t.Cleanup(func() { _ = exec.Command("docker", "rmi", "-f", ref, alias).Run() })
	pushBuild(t, alias, "1")

	env := testenv.New(t)
	env.AddNode("rolling-node", testenv.WithGPUs(1, testenv.GPUSpec{Model: "A100", VRAMMB: 24576}), testenv.WithDriver(dockerdrv.New()))
	env.WaitNodeOnline(t, "rolling-node")
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}
	node := nodes[0]
	image, err := env.Store().UpsertImage(&store.Image{ImageRef: ref, Engine: "vllm", Framework: "cuda", FrameworkVersion: "12.9", Source: "community", RollingTag: true, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	api := env.API(t)
	sync := func(body string) string {
		t.Helper()
		resp, err := api.PostJSON(fmt.Sprintf("/api/v1/images/%d/nodes/%s/sync", image.ID, node.UUID), []byte(body))
		if err != nil {
			t.Fatal(err)
		}
		raw, readErr := io.ReadAll(resp.Body)
		resp.Body.Close()
		if readErr != nil {
			t.Fatal(readErr)
		}
		if resp.StatusCode != http.StatusAccepted {
			t.Fatalf("sync %s: %d %s", body, resp.StatusCode, raw)
		}
		var accepted struct {
			TransferID string `json:"transfer_id"`
		}
		if err := json.Unmarshal(raw, &accepted); err != nil {
			t.Fatal(err)
		}
		return accepted.TransferID
	}
	// staleDigest, when set, is the digest the pull is meant to replace: the row
	// is not settled while it still holds it, because an inventory report can
	// touch the row between the sync being accepted and the transfer being
	// recorded on it, which looks like a finished pull of the old copy.
	settled := func(previous time.Time, transferID, staleDigest string) store.ImageNode {
		t.Helper()
		if transferID == "" {
			t.Fatal("pull accepted without a transfer identity")
		}
		var row store.ImageNode
		env.Eventually(t, 120*time.Second, func() bool {
			got, err := env.Store().ImageNode(image.ID, node.ID)
			if err != nil {
				return false
			}
			row = *got
			if row.TransferID != "" && row.TransferID != transferID {
				t.Fatalf("pull %s replaced by another transfer: %+v", transferID, row)
			}
			// Inventory can report the previous local copy as present during a
			// forced pull. Only the terminal operation clears this transfer ID.
			if row.State == store.ImageStatePresent && staleDigest != "" && row.ImageDigestID == staleDigest {
				return false
			}
			return row.TransferID == "" && (row.State == store.ImageStatePresent || row.State == store.ImageStateFailed) && row.UpdatedAt.After(previous)
		})
		if row.State != store.ImageStatePresent || row.ImageDigestID == "" {
			t.Fatalf("pull did not finish with a digest: %+v", row)
		}
		return row
	}

	firstTransfer := sync(`{}`)
	first := settled(time.Time{}, firstTransfer, "")
	assertImageRevision(t, ref, "1")

	pushBuild(t, alias, "2")
	// A plain request on a present image is answered from the row.
	if transferID := sync(`{}`); transferID != "" {
		t.Fatalf("plain sync started an unexpected transfer: %s", transferID)
	}
	unchanged, err := env.Store().ImageNode(image.ID, node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.ImageDigestID != first.ImageDigestID {
		t.Fatalf("a plain sync re-pulled: %s -> %s", first.ImageDigestID, unchanged.ImageDigestID)
	}

	secondTransfer := sync(`{"force":true}`)
	if secondTransfer == firstTransfer {
		t.Fatalf("forced pull reused transfer %s", firstTransfer)
	}
	second := settled(first.UpdatedAt, secondTransfer, first.ImageDigestID)
	if second.ImageDigestID == first.ImageDigestID {
		t.Fatalf("forced re-pull kept digest %s", first.ImageDigestID)
	}
	if want := imageID(t, ref); second.ImageDigestID != want {
		t.Fatalf("row digest %s, node holds %s", second.ImageDigestID, want)
	}
	assertImageRevision(t, ref, "2")

	// The node table shows it: the series detail's node row carries the new
	// digest.
	resp, err := api.Get(fmt.Sprintf("/api/v1/images/%d", image.SeriesID))
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	var series struct {
		Versions []struct {
			ImageRef string `json:"image_ref"`
			Nodes    []struct {
				NodeUUID      string `json:"node_uuid"`
				ImageDigestID string `json:"image_digest_id"`
				UpdatedAt     string `json:"updated_at"`
			} `json:"nodes"`
		} `json:"versions"`
	}
	if err := json.NewDecoder(resp.Body).Decode(&series); err != nil {
		t.Fatal(err)
	}
	for _, version := range series.Versions {
		if version.ImageRef != ref {
			continue
		}
		for _, row := range version.Nodes {
			if row.NodeUUID == node.UUID && row.ImageDigestID == second.ImageDigestID && row.UpdatedAt != "" {
				return
			}
		}
		t.Fatalf("version nodes = %+v", version.Nodes)
	}
	t.Fatalf("%s missing from its series", ref)
}

func assertImageRevision(t *testing.T, ref, want string) {
	t.Helper()
	out, err := exec.Command("docker", "image", "inspect", "--format", `{{index .Config.Labels "ai.gpuworks.l3.revision"}}`, ref).CombinedOutput()
	if err != nil || strings.TrimSpace(string(out)) != want {
		t.Fatalf("image %s revision = %q, want %s: %v", ref, out, want, err)
	}
}

func waitForRegistry(t *testing.T, container, port string) {
	t.Helper()
	deadline := time.Now().Add(30 * time.Second)
	for time.Now().Before(deadline) {
		if exec.Command("docker", "exec", container, "wget", "-q", "-O", "/dev/null", "http://127.0.0.1:"+port+"/v2/").Run() == nil {
			return
		}
		time.Sleep(200 * time.Millisecond)
	}
	out, _ := exec.Command("docker", "logs", container).CombinedOutput()
	t.Fatalf("local registry did not come up: %s", out)
}

// pushBuild builds a one-label child of the fake engine image, pushes it under
// ref, and removes the local tag so only the registry holds that build.
func pushBuild(t *testing.T, ref, revision string) {
	t.Helper()
	if out, err := buildAndPush(ref, revision); err != nil {
		t.Fatalf("push revision %s: %v: %s", revision, err, out)
	}
}

func buildAndPush(ref, revision string) (string, error) {
	build := exec.Command("docker", "build", "-q", "-t", ref, "-")
	build.Stdin = strings.NewReader("FROM gpuworks/fake-engine:test\nLABEL ai.gpuworks.l3.revision=" + revision + "\n")
	if out, err := build.CombinedOutput(); err != nil {
		return string(out), fmt.Errorf("build: %w", err)
	}
	if out, err := exec.Command("docker", "push", ref).CombinedOutput(); err != nil {
		return string(out), fmt.Errorf("push: %w", err)
	}
	if out, err := exec.Command("docker", "rmi", ref).CombinedOutput(); err != nil {
		return string(out), fmt.Errorf("untag: %w", err)
	}
	return "", nil
}

func imageID(t *testing.T, ref string) string {
	t.Helper()
	out, err := exec.Command("docker", "image", "inspect", "--format", "{{.Id}}", ref).Output()
	if err != nil {
		t.Fatalf("inspect %s: %v", ref, err)
	}
	return strings.TrimSpace(string(out))
}
