package planning

import (
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/parallelism"
	"testing"
)

func TestExtendedLaunchFactsApplyToEveryVendor(t *testing.T) {
	in := Input{Engine: "vllm", Sleepable: true, PD: true, Parallel: parallelism.Spec{PP: 2, NNodes: 2}, Quick: deploy.LaunchConfig{DP: 2, KVDiskOffload: true}}
	for _, vendor := range []string{"nvidia", "ascend", "apple", "amd"} {
		if got := requestedInputFeaturesForVendor(in, vendor); len(got) != 6 {
			t.Fatalf("%s features=%v", vendor, got)
		}
	}
}

// The shared preflight must not restore a sleep request that the effective
// AMD command explicitly disables with its last occurrence.
