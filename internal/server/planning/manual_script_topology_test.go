package planning

import (
	"errors"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A stored deployment whose manual script states one card of the two it is
// assigned (GWKS-415, stored before scripts were judged) still launches, and
// so does the preview the controller trials a node with before it launches;
// the same launch planned for a request that is being written names the
// mismatch.
func TestStoredManualScriptLaunchesWhileAWrittenOneIsRefused(t *testing.T) {
	st, err := store.Open("file:manual-script-topology?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	script := "exec python -m sglang.launch_server --model-path /model --tp-size 1 --host 127.0.0.1 --port 30000"
	request := func(asStored, preview bool) LaunchRequest {
		card := func(index int) store.GPU {
			return store.GPU{Index: index, UUID: "GPU-" + string(rune('a'+index)), Name: "NVIDIA RTX 4090", Vendor: "nvidia", ArchID: "sm_89", ComputeCapability: "8.9", MemoryTotal: 24 << 30}
		}
		return LaunchRequest{
			AsStored: asStored, Preview: preview, APIPort: 31000, Engine: "sglang", Model: &store.Model{ID: 1, WeightBytes: 4 << 30}, Quick: launchConfig(t, deploy.EngineSGLang),
			Node:      &store.Node{ID: 1, UUID: "two-card", OS: "linux", Arch: "amd64", RuntimeDriver: "docker", GPUVendor: "nvidia", GPUs: []store.GPU{card(0), card(1)}},
			Placement: Placement{GPUIndexes: []int{0, 1}, TP: 2},
			Source: LaunchSourceFromDeployment(&store.Deployment{Image: "example/sglang:test",
				ContainerOverrides: `{"command_mode":"manual","cmd_display":"` + script + `"}`}),
			Topology: DistTopology{NNodes: 1}, ModelMount: "/model",
		}
	}
	for _, preview := range []bool{false, true} {
		stored := request(true, preview)
		plan, refusal, err := PlanTemplateLaunch(t.Context(), st, storeNetworks(st), stored.Node, stored, deploy.CatalogSelection{})
		if err != nil || refusal != nil {
			t.Fatalf("stored launch (preview=%v): refusal=%+v err=%v", preview, refusal, err)
		}
		if len(plan.Resolved.Argv) != 2 || !strings.Contains(plan.Resolved.Argv[1], "--tp-size 1") {
			t.Fatalf("argv = %q", plan.Resolved.Argv)
		}
	}
	for _, preview := range []bool{false, true} {
		written := request(false, preview)
		_, _, err = PlanTemplateLaunch(t.Context(), st, storeNetworks(st), written.Node, written, deploy.CatalogSelection{})
		var unresolved *LaunchResolveError
		if !errors.As(err, &unresolved) || !strings.Contains(err.Error(), "sglang topology: tp-size 1 × pp-size 1 requires 1 GPUs, but placement assigned 2") {
			t.Fatalf("written launch (preview=%v) = %v, want the topology mismatch", preview, err)
		}
	}
}
