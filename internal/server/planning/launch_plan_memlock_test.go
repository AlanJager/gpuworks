package planning

import (
	"context"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// memlock_unlimited travels in the deployment's stored snapshot, so the
// controller's launch and the form's preview both carry the ulimit, and the
// deployment's own memlock setting still wins over it.
func TestPlanLaunchTakesTheTemplateMemlockFromTheSnapshot(t *testing.T) {
	st, err := store.Open("file:planning_plan_launch_template_memlock?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/fixture-8b", Name: "fixture", WeightBytes: 4 << 30, Arch: "qwen3"})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("template-memlock-node", store.HashToken("token"), "template-memlock-node")
	if err != nil {
		t.Fatal(err)
	}
	node.GPUs = []store.GPU{{Index: 0, MemoryTotal: 80 << 30, Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0"}}
	for _, tc := range []struct {
		name      string
		snapshot  string
		overrides string
		want      []string
	}{
		{"snapshot asks for unlimited", `"memlock_unlimited":true`, `{}`, []string{"--ulimit", "memlock=-1:-1"}},
		{"snapshot stating false", `"memlock_unlimited":false`, `{}`, nil},
		{"snapshot from before the field", `"weight_bytes":4294967296`, `{}`, nil},
		{"the deployment's own limit wins", `"memlock_unlimited":true`, `{"docker_args":["--ulimit","memlock=4096"]}`, []string{"--ulimit", "memlock=4096"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			selection, err := deploy.DecodeCatalogSelection([]byte(`{"engine":"vllm","spec_name":"deepinfer","spec_source":"upstream",` + tc.snapshot + `}`))
			if err != nil {
				t.Fatal(err)
			}
			dep := &store.Deployment{ID: 13, ModelID: model.ID, NodeID: node.ID, ServedName: "fixture", Engine: "vllm", Image: "registry.example/app:v1", ContainerOverrides: tc.overrides}
			request := LaunchRequest{
				Engine: "vllm", Model: model, Node: node, Selection: selection,
				Placement: Placement{GPUIndexes: []int{0}}, Topology: DistTopology{NNodes: 1, Role: "leader"},
				ModelMount: deploy.DefaultModelMount, ServedName: "fixture", Quick: deploy.LaunchConfig{MaxModelLen: 4096},
			}
			controller := request
			controller.APIPort = 31000
			controller.Source = LaunchSourceFromDeployment(dep)
			preview := request
			preview.Preview = true
			preview.Source = LaunchSourceFromRequest(dep.Image, LaunchSourceFromDeployment(dep).Overrides)
			for name, req := range map[string]LaunchRequest{"controller": controller, "preview": preview} {
				plan, err := PlanLaunch(context.Background(), st, storeNetworks(st), req)
				if err != nil {
					t.Fatalf("%s: %v", name, err)
				}
				if !reflect.DeepEqual(plan.Resolved.DockerArgs, tc.want) {
					t.Fatalf("%s: docker args = %v, want %v", name, plan.Resolved.DockerArgs, tc.want)
				}
			}
		})
	}
}
