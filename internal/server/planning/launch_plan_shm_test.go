package planning

import (
	"context"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The /dev/shm size a template states travels in the deployment's stored
// snapshot, so the controller's launch and the form's preview both carry it,
// and the deployment's own size still wins over it.
func TestPlanLaunchTakesTheTemplateShmFromTheSnapshot(t *testing.T) {
	st, err := store.Open("file:planning_plan_launch_template_shm?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/fixture-8b", Name: "fixture", WeightBytes: 4 << 30, Arch: "qwen3"})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("template-shm-node", store.HashToken("token"), "template-shm-node")
	if err != nil {
		t.Fatal(err)
	}
	node.GPUs = []store.GPU{{Index: 0, MemoryTotal: 80 << 30, Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0"}}
	for _, tc := range []struct {
		name      string
		snapshot  string
		overrides string
		want      int64
	}{
		{"template above the engine default", `"shm_bytes":429496729600`, `{}`, 429496729600},
		{"template below the engine default", `"shm_bytes":4294967296`, `{}`, 16 << 30},
		{"snapshot from before the field", `"weight_bytes":4294967296`, `{}`, 16 << 30},
		{"the deployment's own size wins", `"shm_bytes":429496729600`, `{"shm_bytes":34359738368}`, 32 << 30},
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
				if plan.Resolved.ShmBytes != tc.want {
					t.Fatalf("%s: shm = %d, want %d", name, plan.Resolved.ShmBytes, tc.want)
				}
			}
		})
	}
}
