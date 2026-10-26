//go:build docker

package l3

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	agentruntime "gpuworks.ai/gpuworks/internal/agent/runtime"
	dockerdrv "gpuworks.ai/gpuworks/internal/agent/runtime/docker"
)

// This exercises the real Docker boundary on hosts without a DCU. The character
// devices are harmless stand-ins: native vdev creation and HIP quota enforcement
// belong to the Z100 L4 suite.
func TestHygonDockerPartitionMountsAndDevicesStayInstanceScoped(t *testing.T) {
	if err := exec.Command("docker", "info").Run(); err != nil {
		skipDocker(t, err)
	}
	_, file, _, _ := runtime.Caller(0)
	if err := ensureFakeImage(t, filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))); err != nil {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatal(err)
		}
		t.Skip(err)
	}

	root := t.TempDir()
	dtk, hyhal := filepath.Join(root, "dtk"), filepath.Join(root, "hyhal")
	for _, dir := range []string{dtk, hyhal} {
		if err := os.MkdirAll(dir, 0o755); err != nil {
			t.Fatal(err)
		}
	}
	lock := filepath.Join(root, ".dmi_virtual.lock")
	if err := os.WriteFile(lock, nil, 0o666); err != nil {
		t.Fatal(err)
	}
	driver := dockerdrv.New()
	containers := make([]string, 0, 2)
	t.Cleanup(func() {
		for _, id := range containers {
			_ = exec.Command("docker", "rm", "-f", id).Run()
		}
	})
	for _, item := range []struct {
		id, memory, cus int
	}{{4, 1536, 3}, {5, 2000, 4}} {
		instanceDir := filepath.Join(root, fmt.Sprintf("instance-%d", item.id))
		if err := os.MkdirAll(instanceDir, 0o755); err != nil {
			t.Fatal(err)
		}
		configName := fmt.Sprintf("vdev%d.conf", item.id)
		snapshot := filepath.Join(instanceDir, configName)
		// Z100 vendor configs contain two cu_mask rows; preserving that shape
		// catches consumers that incorrectly assume each key is unique.
		config := hygonL3VendorConfig(item.id, item.memory, item.cus)
		if err := os.WriteFile(snapshot, []byte(config), 0o644); err != nil {
			t.Fatal(err)
		}
		if info, err := os.Stat(snapshot); err != nil || info.Mode().Perm()&0o004 == 0 {
			t.Fatalf("snapshot must be readable by a non-root container: %v, %v", info, err)
		}
		name := fmt.Sprintf("gpuworks-hygon-l3-%s-%d", filepath.Base(filepath.Dir(root)), item.id)
		spec := agentruntime.WorkloadSpec{
			Name: name, Image: "gpuworks/fake-engine:test",
			Env: map[string]string{
				"HYGON_VISIBLE_DEVICES": "0", "HIP_VISIBLE_DEVICES": "0",
				"LD_LIBRARY_PATH": "/opt/dtk/lib:/opt/dtk/lib64:/opt/hyhal/lib",
			},
			// Docker sees real character devices, while their target paths
			// model the exact three-device Hygon contract without host DCU access.
			DockerArgs: []string{"--network", "none", "--device", "/dev/null:/dev/kfd:rwm", "--device", "/dev/zero:/dev/mkfd:rwm", "--device", "/dev/full:/dev/dri/renderD128:rwm", "--group-add", "0"},
			Mounts: []agentruntime.Mount{
				{Source: snapshot, Target: "/etc/vdev/" + configName, ReadOnly: true},
				{Source: snapshot, Target: "/etc/vdev/docker/vdev0.conf", ReadOnly: true},
				{Source: lock, Target: "/tmp/.dmi_virtual.lock"},
				{Source: dtk, Target: "/opt/hygondriver", ReadOnly: true},
				{Source: hyhal, Target: "/opt/hyhal", ReadOnly: true},
			},
		}
		id, err := driver.Run(t.Context(), spec)
		if err != nil {
			t.Fatal(err)
		}
		containers = append(containers, id)
		inspectHygonDockerPartition(t, id, snapshot, configName, config)
	}
	for i, other := range []string{"vdev5.conf", "vdev4.conf"} {
		if out, err := exec.Command("docker", "cp", containers[i]+":/etc/vdev/"+other, filepath.Join(t.TempDir(), other)).CombinedOutput(); err == nil {
			t.Fatalf("instance %d can read another instance config: %s", i, out)
		}
	}
	// Deleting one container must not remove or alter the other instance's
	// snapshot; the native helper owns vdev cleanup, not Docker.
	if err := driver.Remove(t.Context(), containers[0]); err != nil {
		t.Fatal(err)
	}
	if _, err := driver.InspectWorkload(t.Context(), containers[0]); err == nil {
		t.Fatal("deleted instance remains inspectable")
	}
	if got, err := os.ReadFile(filepath.Join(root, "instance-5", "vdev5.conf")); err != nil || !strings.Contains(string(got), "mem: 2000 MiB") {
		t.Fatalf("other instance snapshot changed: %q, %v", got, err)
	}
	inspectHygonDockerPartition(t, containers[1], filepath.Join(root, "instance-5", "vdev5.conf"), "vdev5.conf", "mem: 2000 MiB")
}

func inspectHygonDockerPartition(t *testing.T, id, snapshot, configName, config string) {
	t.Helper()
	out, err := exec.Command("docker", "inspect", id).Output()
	if err != nil {
		t.Fatal(err)
	}
	var rows []struct {
		Config     struct{ Env []string }
		HostConfig struct {
			Privileged bool
			Devices    []struct{ PathOnHost, PathInContainer, CgroupPermissions string }
		}
		Mounts []struct {
			Source, Destination string
			RW                  bool
		}
	}
	if err := json.Unmarshal(out, &rows); err != nil || len(rows) != 1 {
		t.Fatalf("docker inspect: %v, %s", err, out)
	}
	row := rows[0]
	if row.HostConfig.Privileged || len(row.HostConfig.Devices) != 3 {
		t.Fatalf("unexpected privilege/devices: %+v", row.HostConfig)
	}
	wantDevices := map[string]string{"/dev/kfd": "/dev/null", "/dev/mkfd": "/dev/zero", "/dev/dri/renderD128": "/dev/full"}
	for _, device := range row.HostConfig.Devices {
		if wantDevices[device.PathInContainer] != device.PathOnHost || device.CgroupPermissions != "rwm" {
			t.Fatalf("unexpected device: %+v", device)
		}
		delete(wantDevices, device.PathInContainer)
	}
	if len(wantDevices) != 0 {
		t.Fatalf("missing Hygon devices: %v", wantDevices)
	}
	mounts := map[string]struct {
		source string
		rw     bool
	}{}
	for _, mount := range row.Mounts {
		mounts[mount.Destination] = struct {
			source string
			rw     bool
		}{mount.Source, mount.RW}
	}
	for _, target := range []string{"/etc/vdev/" + configName, "/etc/vdev/docker/vdev0.conf"} {
		if got := mounts[target]; got.source != snapshot || got.rw {
			t.Fatalf("config mount %s = %+v, want read-only %s", target, got, snapshot)
		}
	}
	for _, target := range []string{"/opt/hygondriver", "/opt/hyhal"} {
		if got := mounts[target]; got.source == "" || got.rw {
			t.Fatalf("runtime mount %s = %+v", target, got)
		}
	}
	if mounts["/tmp/.dmi_virtual.lock"].source == "" || !mounts["/tmp/.dmi_virtual.lock"].rw {
		t.Fatalf("vendor lock mount = %+v", mounts["/tmp/.dmi_virtual.lock"])
	}
	env := strings.Join(row.Config.Env, "\n")
	for _, want := range []string{"HYGON_VISIBLE_DEVICES=0", "HIP_VISIBLE_DEVICES=0", "LD_LIBRARY_PATH=/opt/dtk/lib:/opt/dtk/lib64:/opt/hyhal/lib"} {
		if !strings.Contains(env, want) {
			t.Fatalf("missing env %q in %s", want, env)
		}
	}
	target := filepath.Join(t.TempDir(), "config")
	if out, err := exec.Command("docker", "cp", id+":/etc/vdev/docker/vdev0.conf", target).CombinedOutput(); err != nil {
		t.Fatalf("read container config: %v: %s", err, out)
	}
	got, err := os.ReadFile(target)
	if err != nil || !strings.Contains(string(got), config) {
		t.Fatalf("container config = %q, err=%v; want %q", got, err, config)
	}
}

func TestHygonDockerRunFailureKeepsSnapshotForRecovery(t *testing.T) {
	if err := exec.Command("docker", "info").Run(); err != nil {
		skipDocker(t, err)
	}
	_, file, _, _ := runtime.Caller(0)
	if err := ensureFakeImage(t, filepath.Clean(filepath.Join(filepath.Dir(file), "../.."))); err != nil {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatal(err)
		}
		t.Skip(err)
	}
	snapshot := filepath.Join(t.TempDir(), "vdev7.conf")
	const failedID, failedMemory, failedCUs = 7, 1536, 3
	failedConfig := hygonL3VendorConfig(failedID, failedMemory, failedCUs)
	if err := os.WriteFile(snapshot, []byte(failedConfig), 0o644); err != nil {
		t.Fatal(err)
	}
	spec := agentruntime.WorkloadSpec{
		Name:       "gpuworks-hygon-l3-fail-" + filepath.Base(filepath.Dir(filepath.Dir(snapshot))),
		Image:      "gpuworks/fake-engine:test",
		Mounts:     []agentruntime.Mount{{Source: snapshot, Target: "/etc/vdev/docker/vdev0.conf", ReadOnly: true}},
		DockerArgs: []string{"--network", "none", "--device", "/definitely-missing-hygon-device:/dev/kfd:rwm"},
	}
	driver := dockerdrv.New()
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", spec.Name).Run() })
	if id, err := driver.Run(t.Context(), spec); err == nil || id != "" {
		t.Fatalf("invalid device must fail Docker run: id=%q err=%v", id, err)
	}
	if got, err := os.ReadFile(snapshot); err != nil || string(got) != failedConfig {
		t.Fatalf("snapshot lost after failed Docker run: %q, %v", got, err)
	}
	if out, err := exec.Command("docker", "inspect", spec.Name).CombinedOutput(); err == nil {
		t.Fatalf("failed Docker run left a container: %s", out)
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if id, err := driver.Run(ctx, spec); err == nil || id != "" {
		t.Fatalf("cancelled Docker run must fail: id=%q err=%v", id, err)
	}
	spec.DockerArgs = []string{"--network", "none", "--device", "/dev/null:/dev/kfd:rwm"}
	id, err := driver.Run(t.Context(), spec)
	if err != nil {
		t.Fatalf("retry after failed launch: %v", err)
	}
	if got, err := os.ReadFile(snapshot); err != nil || string(got) != failedConfig {
		t.Fatalf("retry changed owned snapshot: %q, %v", got, err)
	}
	if err := driver.Remove(t.Context(), id); err != nil {
		t.Fatal(err)
	}
}

func hygonL3VendorConfig(id, memoryMiB, cus int) string {
	return fmt.Sprintf("PciBusId: 0000:07:00.0\ncu_mask: 0x%016x\ncu_mask: 0x0000000000000000\ncu_count: %d\nmem: %d MiB\ndevice_id: 0\nvdev_id: %d\npipe_id: %d\nenable: 1\n", (uint64(1)<<uint(cus))-1, cus, memoryMiB, id, id)
}
