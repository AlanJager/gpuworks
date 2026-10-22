//go:build docker

package l3

import (
	"encoding/json"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// The KV disk tier is the first managed mount the container writes to, and the
// three things that can go wrong with it are all runtime facts: the bind never
// reaches the container, the directory is there but the engine's uid cannot
// write into it, or the ceiling the agent enforces leaks onto the command line
// the user is told they can copy. None of the three is visible without a real
// container.
func TestDockerKVDiskOffloadMountsAWritableManagedDirectory(t *testing.T) {
	fixture := newV4DockerFixture(t, "kv-disk")
	depID, instance := fixture.createDeployment(t, map[string]any{
		"image": fixture.imageRef, "served_name": "l3-kv-disk",
		"quick_params": map[string]any{
			"kv_offloading_size": 8,
			"kv_disk_offload":    true,
			"shared_kv_dir":      "/mnt/kv",
			// Deliberately unstated: the ceiling then comes from the model's
			// weight bytes (1 GiB in the fixture), which is the path a deployer
			// who never opens the advanced section takes.
		},
	})

	// The agent's own directory for this deployment, mounted where the engine
	// was told to write. The docker driver keeps its managed mounts under the
	// agent state directory.
	hostDir := filepath.Join(fixture.agent.StateDir(), "kv-disk", strconv.FormatUint(uint64(depID), 10))
	mounts := containerMounts(t, instance.ContainerID)
	if got := mounts["/mnt/kv"]; got != hostDir {
		t.Fatalf("/mnt/kv is backed by %q, want the managed directory %q; mounts=%v", got, hostDir, mounts)
	}
	if _, err := os.Stat(hostDir); err != nil {
		t.Fatalf("stat %s: %v", hostDir, err)
	}

	if !mountIsWritable(t, instance.ContainerID, "/mnt/kv") {
		t.Fatal("/mnt/kv is bound read-only; the tier has to write block files into it")
	}
	// The engine process itself wrote into the managed directory. Every other
	// managed purpose publishes read-only assets, so nothing before this ever
	// proved a container could create a file inside one - and reaching a running
	// state at all already means the tier's startup write succeeded, because the
	// engine exits when it cannot make one.
	if _, err := os.Stat(filepath.Join(hostDir, "config.json")); err != nil {
		t.Fatalf("the engine's tier configuration did not land in the managed directory: %v", err)
	}

	// ADR 0021 decision 3, against the argv the container really received: the
	// tier is configured and its ceiling is not on the command line.
	argv := containerArgv(t, instance.ContainerID)
	transfer := launchArgValues(argv, "--kv-transfer-config")
	if len(transfer) != 1 || !strings.Contains(transfer[0], `"root_dir":"/mnt/kv"`) {
		t.Fatalf("--kv-transfer-config=%v, want one config naming /mnt/kv; argv=%v", transfer, argv)
	}
	for _, token := range argv {
		if strings.Contains(token, "kv_disk_limit_gib") || strings.Contains(token, "kv-disk-limit") {
			t.Fatalf("the platform ceiling reached the container command line: %v", argv)
		}
	}
	// The ceiling itself still travelled, on the mount the agent acts on.
	if hint := instanceKVDiskSizeHint(t, fixture, instance.ContainerID); hint != int64(2)<<30 {
		t.Fatalf("size hint = %d, want twice the model's 1 GiB of weights", hint)
	}

	// Deleting the deployment releases the disk it was holding.
	fixture.deleteDeployment(t, depID)
	fixture.env.Eventually(t, 45*time.Second, func() bool {
		_, err := os.Stat(hostDir)
		return os.IsNotExist(err)
	})
}

// mountIsWritable reports the runtime's own view of the bind, which is the only
// place a read-only managed mount would show up.
func mountIsWritable(t *testing.T, containerID, target string) bool {
	t.Helper()
	out, err := exec.Command("docker", "inspect", "-f", "{{json .Mounts}}", containerID).Output()
	if err != nil {
		t.Fatalf("docker inspect mounts: %v", err)
	}
	var mounts []struct {
		Destination string `json:"Destination"`
		RW          bool   `json:"RW"`
	}
	if err := json.Unmarshal(out, &mounts); err != nil {
		t.Fatalf("decode mounts %s: %v", out, err)
	}
	for _, mount := range mounts {
		if mount.Destination == target {
			return mount.RW
		}
	}
	t.Fatalf("no mount at %s: %s", target, out)
	return false
}

// containerMounts maps each container path to the host path behind it.
func containerMounts(t *testing.T, containerID string) map[string]string {
	t.Helper()
	out, err := exec.Command("docker", "inspect", "-f", "{{json .Mounts}}", containerID).Output()
	if err != nil {
		t.Fatalf("docker inspect mounts: %v", err)
	}
	var mounts []struct {
		Source      string `json:"Source"`
		Destination string `json:"Destination"`
	}
	if err := json.Unmarshal(out, &mounts); err != nil {
		t.Fatalf("decode mounts %s: %v", out, err)
	}
	byTarget := map[string]string{}
	for _, mount := range mounts {
		byTarget[mount.Destination] = mount.Source
	}
	return byTarget
}

// instanceKVDiskSizeHint reads the ceiling off the persisted desired state, which
// is what the agent's sweeper reads too.
func instanceKVDiskSizeHint(t *testing.T, fixture v4DockerFixture, containerID string) int64 {
	t.Helper()
	instances, err := fixture.env.Store().ListInstances()
	if err != nil {
		t.Fatal(err)
	}
	for _, instance := range instances {
		if instance.ContainerID != containerID {
			continue
		}
		var desired proto.InstanceDesiredState
		if err := json.Unmarshal([]byte(instance.DesiredStateJSON), &desired); err != nil {
			t.Fatalf("decode desired state %s: %v", instance.DesiredStateJSON, err)
		}
		for _, mount := range desired.Mounts {
			if mount.Purpose != proto.MountPurposeKVDisk {
				continue
			}
			hint, convErr := strconv.ParseInt(mount.SizeHint, 10, 64)
			if convErr != nil {
				t.Fatalf("size hint %q: %v", mount.SizeHint, convErr)
			}
			return hint
		}
		t.Fatalf("the instance carries no kv-disk mount: %s", instance.DesiredStateJSON)
	}
	t.Fatalf("no instance for container %s", containerID)
	return 0
}
