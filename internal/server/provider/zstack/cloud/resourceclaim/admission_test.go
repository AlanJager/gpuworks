package resourceclaim

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/inventory"
)

func TestIsAllocatableGPURequiresEnabledAndUnallocated(t *testing.T) {
	for name, gpu := range map[string]inventory.CloudGPU{
		"free":         {State: "Enabled", Allocation: "Unallocated"},
		"allocated":    {State: "Enabled", Allocation: "Allocated"},
		"disabled":     {State: "Disabled", Allocation: "Unallocated"},
		"both invalid": {State: "Disabled", Allocation: "Allocated"},
	} {
		want := name == "free"
		if got := isAllocatableGPU(gpu); got != want {
			t.Errorf("%s: allocatable = %v, want %v", name, got, want)
		}
	}
}
