package engine

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/agent/gpu"
	"gpuworks.ai/gpuworks/internal/assets"
)

func TestAscendProbePreservesSelectedPhysicalDevice(t *testing.T) {
	for _, driverName := range []string{"docker", "nerdctl", "udocker"} {
		t.Run(driverName, func(t *testing.T) {
			root := t.TempDir()
			assetDir := filepath.Join(root, "dgpu-runtime", "ascend", "linux-"+runtime.GOARCH)
			if err := os.MkdirAll(assetDir, 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(assetDir, "libvruntime.so"), fakeELF(runtime.GOARCH), 0o755); err != nil {
				t.Fatal(err)
			}
			prober := &ascendPartitionTestProber{FixedProber: &gpu.FixedProber{VendorName: "ascend"}}
			e := &Engine{stateDir: root, prober: prober, driver: &headlessTestDriver{name: driverName}}
			ws, err := e.BuildPartitionProbeWorkload(context.Background(), accelerator.BackendAscendLogical, assets.PartitionProbeImage, 6, 4<<30, "probe-device")
			if err != nil {
				t.Fatal(err)
			}
			if ws.Env["GPUWORKS_PARTITION_PROBE_DEVICE"] != "6" || ws.Env["GPUWORKS_PARTITION_PROBE_QUOTA_MIB"] != "4096" {
				t.Fatalf("probe environment = %v", ws.Env)
			}
			if len(ws.GPUIndexes) != 1 || ws.GPUIndexes[0] != 6 || len(prober.shareCalls) != 1 || prober.shareCalls[0] != 6 {
				t.Fatalf("workload devices=%v share requests=%v", ws.GPUIndexes, prober.shareCalls)
			}
			config, err := os.ReadFile(filepath.Join(root, "dgpu-runtime", "instances", "probe-device", "npu_info.config"))
			if err != nil || !strings.Contains(string(config), "physical-npu-id=6\n") || !strings.Contains(string(config), "memory-quota=4096\n") {
				t.Fatalf("quota config=%s err=%v", config, err)
			}
		})
	}
}
