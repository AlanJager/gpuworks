//go:build docker

package engine

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	agentruntime "gpuworks.ai/gpuworks/internal/agent/runtime"
	dockerdrv "gpuworks.ai/gpuworks/internal/agent/runtime/docker"
	"gpuworks.ai/gpuworks/internal/assets"
	"gpuworks.ai/gpuworks/internal/proto"
)

// The agent's probe builder and deployment injector must produce mounts that
// real Docker accepts even on a host with no DCU. HIP quota behavior is L4.
func TestHygonProbeAndDeploymentDockerBoundary(t *testing.T) {
	if err := exec.Command("docker", "info").Run(); err != nil {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatal(err)
		}
		t.Skipf("Docker unavailable: %v", err)
	}
	const image = "gpuworks/fake-engine:test"
	if err := exec.Command("docker", "image", "inspect", image).Run(); err != nil {
		if os.Getenv("GPUWORKS_TEST_STRICT") == "1" {
			t.Fatal(err)
		}
		t.Skipf("L3 fixture image unavailable: %v", err)
	}
	t.Setenv("GPUWORKS_FAKE_GPU_NO_INJECT", "1")
	e, h, helper := hygonTestEngine(t)
	driver := dockerdrv.New()
	e.driver = driver
	previous := partitionInjectors[accelerator.BackendHygonLogical]
	partitionInjectors[accelerator.BackendHygonLogical] = h
	t.Cleanup(func() { partitionInjectors[accelerator.BackendHygonLogical] = previous })

	probe, err := e.BuildPartitionProbeWorkload(t.Context(), accelerator.BackendHygonLogical, assets.PartitionProbeImage, 0, 1536<<20, "hygon-l3-probe")
	if err != nil {
		t.Fatal(err)
	}
	if probe.Env["GPUWORKS_PARTITION_PROBE_QUOTA_MIB"] != "1536" || probe.Env["GPUWORKS_PARTITION_PROBE_BACKEND"] != accelerator.BackendHygonLogical {
		t.Fatalf("probe contract = %+v", probe.Env)
	}
	if !hygonDockerMountsMatch(probe.Mounts, "vdev4.conf", true) {
		t.Fatalf("probe mounts = %+v", probe.Mounts)
	}
	// A static fixture image exercises Docker's mounts. It intentionally does
	// not masquerade as a HIP probe or claim the dGPU capability is ready.
	probe.Image = image
	// These assertions concern mounts and device ownership; no network is
	// needed, including on CI hosts without a configured Docker bridge.
	probe.DockerArgs = append(probe.DockerArgs, "--network", "none")
	probe.Name = fmt.Sprintf("gpuworks-hygon-probe-l3-%d", os.Getpid())
	probe.Cmd = []string{"--model", "/model", "--port", "49993"}
	id, err := driver.Run(t.Context(), probe)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = exec.Command("docker", "rm", "-f", id).Run() })
	var inspected []struct {
		Mounts []struct{ Destination string }
	}
	data, err := exec.Command("docker", "inspect", id).Output()
	if err != nil {
		t.Fatalf("inspect probe container: %v: %s", err, data)
	}
	if err := json.Unmarshal(data, &inspected); err != nil || len(inspected) != 1 {
		t.Fatalf("decode probe container: %v: %s", err, data)
	}
	targets := map[string]bool{}
	for _, mount := range inspected[0].Mounts {
		targets[mount.Destination] = true
	}
	for _, target := range []string{"/etc/vdev/vdev4.conf", "/etc/vdev/docker/vdev0.conf", "/opt/dtk", "/opt/hygondriver", "/tmp/.dmi_virtual.lock"} {
		if !targets[target] {
			t.Fatalf("Docker omitted probe mount %s: %v", target, targets)
		}
	}
	if err := driver.Remove(t.Context(), id); err != nil {
		t.Fatal(err)
	}
	if err := e.CleanupPartitionProbe("hygon-l3-probe"); err != nil {
		t.Fatal(err)
	}
	if helper.owned[4] || hygonConfigExists(t, h.vdevDir, "vdev4.conf") {
		t.Fatal("probe vdev survived cleanup")
	}

	deployment := &agentruntime.WorkloadSpec{Image: image, Env: map[string]string{}}
	if ok, reason := h.apply(e, t.Context(), deployment, proto.InstanceDesiredState{UUID: "hygon-l3-deployment", GPUIndexes: []int{0}, VRAMLimitBytes: 1536 << 20}); !ok {
		t.Fatal(reason)
	}
	if !hygonDockerMountsMatch(deployment.Mounts, "vdev5.conf", false) {
		t.Fatalf("deployment mounts = %+v", deployment.Mounts)
	}
	if err := h.cleanupInstance(e, "hygon-l3-deployment"); err != nil {
		t.Fatal(err)
	}

	missing, err := e.BuildPartitionProbeWorkload(t.Context(), accelerator.BackendHygonLogical, assets.PartitionProbeImage, 0, 1536<<20, "hygon-l3-missing")
	if err != nil {
		t.Fatal(err)
	}
	missing.Name = fmt.Sprintf("gpuworks-hygon-missing-l3-%d", os.Getpid())
	missing.Image = fmt.Sprintf("gpuworks/missing-hygon-probe:%d", os.Getpid())
	missing.DockerArgs = append(missing.DockerArgs, "--pull=never")
	if got, runErr := driver.Run(context.Background(), missing); runErr == nil || got != "" {
		t.Fatalf("missing probe image launched: id=%q err=%v", got, runErr)
	}
	if err := e.CleanupPartitionProbe("hygon-l3-missing"); err != nil {
		t.Fatal(err)
	}
	if helper.owned[6] || hygonConfigExists(t, h.vdevDir, "vdev6.conf") {
		t.Fatal("failed probe vdev survived cleanup")
	}
}

func hygonDockerMountsMatch(mounts []agentruntime.Mount, configName string, probe bool) bool {
	targets := map[string]agentruntime.Mount{}
	for _, mount := range mounts {
		targets[mount.Target] = mount
	}
	host, container := targets["/etc/vdev/"+configName], targets["/etc/vdev/docker/vdev0.conf"]
	if host.Source == "" || host.Source != container.Source || !host.ReadOnly || !container.ReadOnly {
		return false
	}
	for _, target := range []string{"/opt/hygondriver", "/opt/hyhal"} {
		if targets[target].Source == "" || !targets[target].ReadOnly {
			return false
		}
	}
	return targets["/tmp/.dmi_virtual.lock"].Source != "" && (targets["/opt/dtk"].Source != "") == probe
}

func hygonConfigExists(t *testing.T, dir, name string) bool {
	t.Helper()
	_, err := os.Stat(filepath.Join(dir, name))
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return err == nil
}
