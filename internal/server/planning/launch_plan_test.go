package planning

import (
	"context"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/network"
	"gpuworks.ai/gpuworks/internal/server/store"
	"reflect"
	"strings"
	"testing"
)

// PlanLaunch is the one assembly both callers use, so its estimate input has to
// carry the catalog facts whoever asks: the spec's weight, the loader layout's
// resident weight, the measured KV cost and the node's cache record. The
// controller used to omit the last three, which is why the breakdown it
// persisted could disagree with what the form had shown.
func TestPlanLaunchSpendsTheSameCatalogFactsForEveryCaller(t *testing.T) {
	st, err := store.Open("file:planning_plan_launch?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{{
		ModelID: "org/fixture-8b", Source: "upstream", Architecture: "qwen3",
		KVBytesPerToken: `{"vllm":262144}`,
		Specs: []store.InferenceSpecRow{{
			Name: "balanced", Engine: "vllm", Image: "registry.example/app:v1", Quantization: "bf16",
			WeightBytes: 8 << 30, RuntimeWeightBytes: 6 << 30,
			WeightSources: `[{"provider":"huggingface","repo_id":"org/fixture-8b"}]`,
			GPUs:          `{"vendor":"nvidia","min_count":1}`,
		}},
	}}); err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("plan-launch-node", store.HashToken("token"), "plan-launch-node")
	if err != nil {
		t.Fatal(err)
	}
	node.GPUs = []store.GPU{{Index: 0, MemoryTotal: 80 << 30, Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0"}}
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/fixture-8b", Name: "fixture", WeightBytes: 0, Arch: "qwen3"})
	if err != nil {
		t.Fatal(err)
	}
	selection, err := deploy.DecodeCatalogSelection([]byte(`{"engine":"vllm","spec_name":"balanced","spec_source":"upstream","weight_bytes":8589934592,"runtime_weight_bytes":6442450944,"gpus":{"vendor":"nvidia","min_count":1},"extra_args":["--dtype","bfloat16"]}`))
	if err != nil {
		t.Fatal(err)
	}
	plan, err := PlanLaunch(context.Background(), st, storeNetworks(st), LaunchRequest{
		Engine: "vllm", Model: model, Node: node, Selection: selection,
		Placement: Placement{GPUIndexes: []int{0}}, Topology: DistTopology{NNodes: 1},
		Source:     LaunchSourceFromDeployment(&store.Deployment{ID: 5, Image: "registry.example/app:v1"}),
		ModelMount: deploy.DefaultModelMount, ServedName: "fixture", APIPort: 30000,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !plan.Output.Result.OK || plan.Output.Result.Plan == nil {
		t.Fatalf("plan did not fit: %+v", plan.Output.Result)
	}
	// The catalog's measured KV cost reaches the breakdown: a token capacity is
	// only claimed with it.
	if plan.Output.Breakdown.EstimatedKVTokens <= 0 || plan.Output.Breakdown.KVEstimateConfidence != "measured" {
		t.Fatalf("breakdown = %+v", plan.Output.Breakdown)
	}
	if plan.Params.TensorParallel != 1 || plan.Params.AssignedGPUs != 1 || plan.Params.ModelPath != deploy.DefaultModelMount {
		t.Fatalf("params = %+v", plan.Params)
	}
	if plan.Resolved.ModelMount != deploy.DefaultModelMount || len(plan.Resolved.Argv) == 0 {
		t.Fatalf("resolved = %+v", plan.Resolved)
	}
	if !strings.Contains(strings.Join(plan.Resolved.Argv, " "), "--dtype bfloat16") {
		t.Fatalf("template arguments did not reach the command: %v", plan.Resolved.Argv)
	}
	// An assignment the node cannot satisfy is a plan that does not fit, not an
	// error and never a launch on some other card: the caller reports it.
	unfittable, err := PlanLaunch(context.Background(), st, storeNetworks(st), LaunchRequest{
		Engine: "vllm", Model: model, Node: node, Selection: selection,
		Placement: Placement{GPUIndexes: []int{3}}, Topology: DistTopology{NNodes: 1},
		Source: LaunchSourceFromDeployment(&store.Deployment{ID: 5, Image: "registry.example/app:v1"}),
	})
	if err != nil {
		t.Fatal(err)
	}
	if unfittable.Output.Result.OK || unfittable.Output.Result.Plan != nil || len(unfittable.Resolved.Argv) != 0 {
		t.Fatalf("unfittable assignment produced a launch: %+v", unfittable.Output.Result)
	}
}

// The acceptance the plan asks for: one deployment row, planned once the way the
// controller plans it and once the way the preview does, has to come out the
// same - same estimate input, same params but for the port a preview cannot
// reserve and the GPU count only a schedule can settle.
func TestPlanLaunchAgreesBetweenTheControllerAndThePreview(t *testing.T) {
	st, err := store.Open("file:planning_plan_launch_parity?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/fixture-8b", Name: "fixture", WeightBytes: 4 << 30, Arch: "qwen3"})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("parity-node", store.HashToken("token"), "parity-node")
	if err != nil {
		t.Fatal(err)
	}
	node.GPUs = []store.GPU{{Index: 0, MemoryTotal: 80 << 30, Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0"}}
	selection, err := deploy.DecodeCatalogSelection([]byte(`{"engine":"vllm","spec_name":"balanced","spec_source":"upstream","weight_bytes":4294967296,"runtime_weight_bytes":4294967296,"extra_args":["--dtype","bfloat16"]}`))
	if err != nil {
		t.Fatal(err)
	}
	dep := &store.Deployment{ID: 11, ModelID: model.ID, NodeID: node.ID, ServedName: "fixture", Engine: "vllm", Image: "registry.example/app:v1", ContainerOverrides: `{"env":{"USER":"1"}}`}
	request := LaunchRequest{
		Engine: "vllm", Model: model, Node: node, Selection: selection,
		Placement: Placement{GPUIndexes: []int{0}}, Topology: DistTopology{NNodes: 1, Role: "leader"},
		Quick: deploy.LaunchConfig{MaxModelLen: 4096}, ExtraArgs: []string{"--enable-metrics"},
		ModelMount: deploy.DefaultModelMount, ServedName: "fixture",
	}
	controller := request
	controller.APIPort = 31000
	controller.Source = LaunchSourceFromDeployment(dep)
	controllerPlan, err := PlanLaunch(context.Background(), st, storeNetworks(st), controller)
	if err != nil {
		t.Fatal(err)
	}
	preview := request
	preview.Preview = true
	preview.Source = LaunchSourceFromRequest(dep.Image, LaunchSourceFromDeployment(dep).Overrides)
	previewPlan, err := PlanLaunch(context.Background(), st, storeNetworks(st), preview)
	if err != nil {
		t.Fatal(err)
	}
	if !controllerPlan.Output.Result.OK || !previewPlan.Output.Result.OK {
		t.Fatalf("controller=%+v preview=%+v", controllerPlan.Output.Result, previewPlan.Output.Result)
	}
	// Only the controller's launch names its deployment: a preview's has
	// none yet, or an edit's is excluded whole (ExcludeDeploymentID).
	if controllerPlan.Input.DeploymentID != dep.ID || previewPlan.Input.DeploymentID != 0 {
		t.Fatalf("deployment ids = %d, %d", controllerPlan.Input.DeploymentID, previewPlan.Input.DeploymentID)
	}
	previewPlan.Input.DeploymentID = dep.ID
	if !reflect.DeepEqual(controllerPlan.Input, previewPlan.Input) {
		t.Fatalf("inputs differ:\n controller %+v\n preview    %+v", controllerPlan.Input, previewPlan.Input)
	}
	controllerPlan.Params.APIPort, previewPlan.Params.APIPort = 0, 0
	controllerPlan.Params.AssignedGPUs, previewPlan.Params.AssignedGPUs = 0, 0
	if !reflect.DeepEqual(controllerPlan.Params, previewPlan.Params) {
		t.Fatalf("params differ:\n controller %+v\n preview    %+v", controllerPlan.Params, previewPlan.Params)
	}
	if controllerPlan.Source.Image != previewPlan.Source.Image || !reflect.DeepEqual(controllerPlan.Source.Overrides, previewPlan.Source.Overrides) {
		t.Fatalf("sources differ: %+v vs %+v", controllerPlan.Source, previewPlan.Source)
	}
}

// The runveil thread caps come from the node the launch is planned on, so the
// preview the form shows and the launch the controller sends carry the same
// keys, and a key the deployment removed is gone from both.
func TestPlanLaunchCapsSGLangThreadsOnRunveilNodes(t *testing.T) {
	st, err := store.Open("file:planning_plan_launch_runveil?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/fixture-8b", Name: "fixture", WeightBytes: 4 << 30, Arch: "qwen3"})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("runveil-node", store.HashToken("token"), "runveil-node")
	if err != nil {
		t.Fatal(err)
	}
	node.GPUs = []store.GPU{{Index: 0, MemoryTotal: 80 << 30, Vendor: "nvidia", ArchID: "sm_90", ComputeCapability: "9.0"}}
	selection, err := deploy.DecodeCatalogSelection([]byte(`{"engine":"sglang","spec_name":"balanced","spec_source":"upstream","weight_bytes":4294967296,"runtime_weight_bytes":4294967296}`))
	if err != nil {
		t.Fatal(err)
	}
	dep := &store.Deployment{ID: 12, ModelID: model.ID, NodeID: node.ID, ServedName: "fixture", Engine: "sglang", Image: "registry.example/sglang:v1", ContainerOverrides: `{"env_unset":["TORCH_NUM_THREADS"]}`}
	request := LaunchRequest{
		Engine: "sglang", Model: model, Node: node, Selection: selection,
		Placement: Placement{GPUIndexes: []int{0}}, Topology: DistTopology{NNodes: 1, Role: "leader"},
		ModelMount: deploy.DefaultModelMount, ServedName: "fixture",
	}
	for _, tc := range []struct {
		runtime string
		want    string
	}{{"udocker", "4"}, {"docker", ""}} {
		node.RuntimeDriver = tc.runtime
		controller := request
		controller.APIPort = 31000
		controller.Source = LaunchSourceFromDeployment(dep)
		preview := request
		preview.Preview = true
		preview.Source = LaunchSourceFromRequest(dep.Image, LaunchSourceFromDeployment(dep).Overrides)
		for name, req := range map[string]LaunchRequest{"controller": controller, "preview": preview} {
			plan, err := PlanLaunch(context.Background(), st, storeNetworks(st), req)
			if err != nil {
				t.Fatal(err)
			}
			if got := plan.Resolved.Env["OMP_NUM_THREADS"]; got != tc.want {
				t.Fatalf("%s on %s: OMP_NUM_THREADS = %q, want %q (env %v)", name, tc.runtime, got, tc.want, plan.Resolved.Env)
			}
			if _, kept := plan.Resolved.Env["TORCH_NUM_THREADS"]; kept {
				t.Fatalf("%s on %s: removed TORCH_NUM_THREADS is still set", name, tc.runtime)
			}
		}
	}
}

// storeNetworks is the network service over every network in st, as the app
// wires it.
func storeNetworks(st *store.Store) Networks {
	return network.New(st, network.StoreCatalog{Store: st})
}
