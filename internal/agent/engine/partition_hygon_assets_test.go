package engine

import (
	"context"
	"os"
	"path/filepath"
	"slices"
	"testing"

	agentruntime "gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/proto"
)

func TestHygonBWUsesHYHALSMIAndPhysicalComputeUnits(t *testing.T) {
	e, h, helper := hygonTestEngine(t)
	prober := e.prober.(*hygonTestProber)
	prober.GPUs[0].Name = "BW"
	prober.GPUs[0].ArchID = "BW"
	prober.GPUs[0].MemoryTotalBytes = 65520 << 20
	prober.computeUnits = 80
	if err := os.Remove(filepath.Join(h.dtkDir, "lib", "librocm_smi64.so")); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(h.hyhalDir, "lib", "librocm_smi64.so"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	reports := h.assetReports(e)
	if len(reports) != 1 || !slices.Contains(reports[0].Files, "librocm_smi64.so") {
		t.Fatalf("DTK with SMI in HYHAL assets = %+v", reports)
	}
	ws := &agentruntime.WorkloadSpec{}
	ok, why := h.apply(e, context.Background(), ws, proto.InstanceDesiredState{
		UUID: "bw-physical-cus", GPUIndexes: []int{0}, VRAMLimitBytes: 4096 << 20,
	})
	if !ok || !slices.Equal(helper.createdCUs, []int{6}) {
		t.Fatalf("BW quota: ok=%v reason=%s CUs=%v", ok, why, helper.createdCUs)
	}
	if err := h.cleanupInstance(e, "bw-physical-cus"); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(filepath.Join(h.hyhalDir, "lib", "librocm_smi64.so")); err != nil {
		t.Fatal(err)
	}
	if reports := h.assetReports(e); len(reports) != 0 {
		t.Fatalf("missing SMI was reported available: %+v", reports)
	}
}
