package planning

import (
	"context"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestPlanLaunchPassesTheEffectiveImageToCompatibilityResolution(t *testing.T) {
	st, err := store.Open("file:planning_launch_compat?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/compat", Name: "compat", WeightBytes: 4 << 30, Arch: "qwen3"})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("planning-compat-node", store.HashToken("token"), "planning-compat-node")
	if err != nil {
		t.Fatal(err)
	}
	node.RuntimeDriver = "udocker"
	node.GPUs = []store.GPU{{Index: 0, MemoryTotal: 80 << 30, Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0"}}
	selection, err := deploy.DecodeCatalogSelection([]byte(`{"engine":"vllm","spec_name":"balanced","spec_source":"upstream","weight_bytes":4294967296,"runtime_weight_bytes":4294967296}`))
	if err != nil {
		t.Fatal(err)
	}
	const image = "quay.io/ascend/vllm-ascend:v0.30.0rc2-a3"
	request := LaunchRequest{
		Engine: "vllm", Model: model, Node: node, Selection: selection,
		Placement: Placement{GPUIndexes: []int{0}}, Topology: DistTopology{NNodes: 1, Role: "leader"},
		ModelMount: deploy.DefaultModelMount, ServedName: "compat",
	}
	controller := request
	controller.APIPort = 31000
	controller.Source = LaunchSourceFromDeployment(&store.Deployment{ID: 71, Image: image})
	preview := request
	preview.Preview = true
	preview.Source = LaunchSourceFromRequest(image, deploy.ContainerOverrides{})
	for name, req := range map[string]LaunchRequest{"controller": controller, "preview": preview} {
		plan, err := PlanLaunch(context.Background(), st, storeNetworks(st), req)
		if err != nil {
			t.Fatalf("%s: %v", name, err)
		}
		if !strings.Contains(plan.Resolved.PreStartScript, "# GPUWorks: vLLM-Ascend v0.30.0rc2-a3 compatibility") {
			t.Fatalf("%s did not resolve the image compatibility overlay", name)
		}
	}
}
