package partition

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/server/partitioncap"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// TestAMDDGPUQuotaCrossesAPIControllerAgentAndRuntime exercises the AMD
// logical partition backend end to end: node registration reports the
// libvrocm.so asset, the capability becomes ready without a behavioral
// probe (amd-dgpu is probe-exempt — see internal/accelerator/amd.go), and a
// deployment naming amd-dgpu reaches the agent's runtime with the shim
// mounted and the per-card VROCM_DEVICE_MEMORY_LIMIT_<N> quota set. The GPU
// prober and runtime are fakes (testenv.WithoutAcceleratorRuntimeInjection):
// this does not exercise host KFD/render-node injection or real ROCm
// containers.
func TestAMDDGPUQuotaCrossesAPIControllerAgentAndRuntime(t *testing.T) {
	root := t.TempDir()
	assetDir := filepath.Join(root, "amd", "linux-amd64")
	if err := os.MkdirAll(assetDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(assetDir, "libvrocm.so"), []byte("test-runtime-asset"), 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("GPUWORKS_PARTITION_ASSETS", root)

	env := testenv.New(t)
	healthy := true
	driver := testenv.NewFakeDriver("docker")
	t.Cleanup(driver.CloseAll)
	node := env.AddNode("partition-amd",
		testenv.WithGPUList(testenv.GPUSpec{Model: "AMD Instinct MI300X", Vendor: "amd", ArchID: "gfx942", VRAMMB: 196608, Healthy: &healthy}),
		testenv.WithAccelerator("amd", "rocm", "7.2.3"),
		testenv.WithDriver(driver),
		testenv.WithoutAcceleratorRuntimeInjection(),
	)
	env.WaitNodeOnline(t, "partition-amd")
	waitForAssetReport(t, env, node.UUID, accelerator.BackendAMDLogical)
	// amd-dgpu is probe-exempt: readiness never waits on a probe report.
	assertNodeAPICapability(t, env, node.UUID, accelerator.BackendAMDLogical, partitioncap.StatusReady)

	row, err := env.Store().NodeWithGPUs(node.UUID)
	if err != nil {
		t.Fatal(err)
	}
	model := registerPresentModel(t, env, row, "amd")

	runSeen := make(chan runtime.WorkloadSpec, 1)
	driver.AfterRun(func(spec runtime.WorkloadSpec) {
		select {
		case runSeen <- spec:
		default:
		}
	})
	body, _ := json.Marshal(map[string]any{
		"name": "partition-amd", "served_name": "partition-amd",
		"model_id": model.ID, "node_uuid": node.UUID, "image": "validated/amd:test",
		"partition_backend": accelerator.BackendAMDLogical, "vram_limit_gib": 60,
		"quick_params": map[string]any{"max_model_len": 256},
	})
	resp, err := env.API(t).PostJSON("/api/v1/deployments", body)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusCreated {
		raw, _ := io.ReadAll(resp.Body)
		t.Fatalf("create status=%d body=%s", resp.StatusCode, raw)
	}

	var spec runtime.WorkloadSpec
	select {
	case spec = <-runSeen:
	case <-time.After(30 * time.Second):
		t.Fatal("agent did not deliver the partitioned AMD workload to runtime")
	}
	assertMount(t, spec, "/opt/gpuworks/partition/libvrocm.so")
	limitMiB := "61440" // 60 GiB
	if spec.Env["LD_PRELOAD"] != "/opt/gpuworks/partition/libvrocm.so" || spec.Env["VROCM_DEVICE_MEMORY_LIMIT_0"] != limitMiB {
		t.Fatalf("AMD environment = %+v", spec.Env)
	}
	if ledger := spec.Env["VROCM_LEDGER_PATH"]; !strings.HasPrefix(ledger, "/dev/shm/gpuworks-amd-") {
		t.Fatalf("AMD ledger path = %q", ledger)
	}
	for key := range spec.Env {
		if strings.Contains(key, "SM_LIMIT") || strings.Contains(key, "CU_MASK") {
			t.Fatalf("unexpected compute-limit env on a memory-only backend: %q", key)
		}
	}
}
