package planning

import (
	"context"
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// An AMD template selection keeps its declared launch resources and placement
// ordering through the same snapshot-to-launch path used by deployments.
func TestAMDTemplatePreservesResourcesAndPlacementPreference(t *testing.T) {
	st, err := store.Open("file:planning_amd_template_resources?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "Qwen/Qwen3.8-27B", Name: "Qwen3.8-27B",
		Arch: "qwen3_5", WeightBytes: 4 << 30,
		Meta: `{"model_type":"qwen3_5","num_hidden_layers":64,"hidden_size":5120,"num_attention_heads":24,"num_key_value_heads":4,"head_dim":256,"max_position_embeddings":262144}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("amd-template-node", store.HashToken("token"), "amd-template-node")
	if err != nil {
		t.Fatal(err)
	}
	node.OS, node.Arch, node.RuntimeDriver, node.GPUVendor = "linux", "amd64", "docker", "amd"
	node.DiskTotalKB = 1 << 30
	node.GPUs = []store.GPU{{Index: 0, Name: "AMD GPU 0x1586 (gfx1151)", Vendor: "amd", ArchID: "gfx1151", MemoryTotal: 64 << 30}}

	const resolver = `def resolve(gpu):
    if gpu.vendor != "amd" or gpu.count != 1:
        return reject("requires one AMD GPU")
    return accept(gpu_memory_mib_per_device=1024, variables={"GPU_COUNT": 1, "TP_SIZE": 1}, prefer=7)`
	selectionWire, err := json.Marshal(map[string]any{
		"engine": "vllm", "spec_name": "bf16-vllm-amd395-gfx1151-local", "spec_source": "user",
		"image": "gpuworks-local/amd395-vllm:qwen38-r2-20261022", "weight_bytes": int64(4 << 30),
		"gpus":      map[string]string{"vendor": "amd", "name": "(?i)gfx1151"},
		"placement": map[string]string{"language": "starlark", "resolver": resolver},
		"shm_bytes": int64(32 << 30), "memlock_unlimited": true,
		"entrypoint": []string{"vllm", "serve"},
	})
	if err != nil {
		t.Fatal(err)
	}
	selection, err := deploy.DecodeCatalogSelection(selectionWire)
	if err != nil {
		t.Fatal(err)
	}

	request := LaunchRequest{
		Engine: "vllm", Model: model, Node: node, Selection: selection,
		Placement: Placement{GPUIndexes: []int{0}}, Topology: DistTopology{NNodes: 1, Role: "leader"},
		ModelMount: deploy.DefaultModelMount, ServedName: "qwen", APIPort: 31000,
		Quick: launchConfig(t, "vllm"), Source: LaunchSourceFromRequest(selection.Image, deploy.ContainerOverrides{}),
	}
	plan, err := PlanLaunch(context.Background(), st, storeNetworks(st), request)
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Output.Result.OK || plan.Output.Result.Plan == nil {
		t.Fatalf("AMD template placement refused: %+v", plan.Output.Result)
	}
	if plan.Output.Result.Plan.TP != 1 || plan.Output.Result.Plan.Preference != 7 {
		t.Fatalf("AMD placement = TP %d, preference %d; want single GPU with preference 7", plan.Output.Result.Plan.TP, plan.Output.Result.Plan.Preference)
	}
	if plan.Resolved.ShmBytes != 32<<30 {
		t.Fatalf("template shm bytes = %d, want %d", plan.Resolved.ShmBytes, int64(32<<30))
	}
	memlockCount := 0
	for i, arg := range plan.Resolved.DockerArgs {
		if arg == "--ulimit" && i+1 < len(plan.Resolved.DockerArgs) && plan.Resolved.DockerArgs[i+1] == "memlock=-1:-1" {
			memlockCount++
		}
	}
	if memlockCount != 1 {
		t.Fatalf("template memlock ulimit count = %d in %v, want one", memlockCount, plan.Resolved.DockerArgs)
	}
}
