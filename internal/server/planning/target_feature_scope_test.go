package planning

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/parallelism"
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
func TestAMDPlanningUsesLastSleepFlag(t *testing.T) {
	for _, tc := range []struct {
		name           string
		args           []string
		declared, want bool
	}{
		{"last false", []string{"--enable-sleep-mode=true", "--enable-sleep-mode=false"}, false, false},
		{"last true", []string{"--enable-sleep-mode=false", "--enable-sleep-mode=true"}, false, true},
		{"declared sleep", []string{"--enable-sleep-mode=false"}, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			in := Input{Engine: "vllm", Sleepable: tc.declared, RequestedFeatures: deploy.ResolveRequestedFeatures("vllm", deploy.ResolvedLaunch{Entrypoint: []string{"vllm", "serve"}, Argv: tc.args}, deploy.Params{Sleepable: tc.declared})}
			found := false
			for _, feature := range requestedInputFeaturesForVendor(in, accelerator.VendorAMD) {
				found = found || feature == accelerator.FeatureEngineSleep
			}
			if found != tc.want {
				t.Fatalf("sleep feature=%t, want %t", found, tc.want)
			}
		})
	}
}
