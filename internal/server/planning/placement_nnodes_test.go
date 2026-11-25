package planning

import (
	"context"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/parallelism"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Every rank of a multi-node instance is planned on its own node, so a
// resolver that limits the whole instance to one GPU sees one card on each
// rank of a two-node group. The plan tells it how many nodes the instance
// spans, and the rank is refused by the resolver's own reason.
func TestAResolverIsToldHowManyNodesTheInstanceSpans(t *testing.T) {
	st, err := store.Open("file:planning_resolver_nnodes?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("resolver-nnodes", store.HashToken("token"), "resolver-nnodes")
	if err != nil {
		t.Fatal(err)
	}
	node.DiskTotalKB = 1 << 30
	node.GPUs = []store.GPU{{Index: 0, MemoryTotal: 48 << 30, Vendor: "nvidia", ArchID: "sm_86", ComputeCapability: "8.6"}}
	const reason = "runs on one GPU of one node"
	const resolver = `def resolve(gpu):
    if gpu.count != 1 or gpu.nnodes != 1:
        return reject("` + reason + `")
    return {"TP_SIZE": 1}`
	model := &store.Model{Name: "single-gpu", WeightBytes: 4 << 30}
	quick := launchConfig(t, "vllm")
	for _, test := range []struct {
		name       string
		parallel   parallelism.Spec
		wantReject bool
	}{
		{name: "automatic on one node", parallel: parallelism.Spec{}},
		{name: "one node, one card stated", parallel: parallelism.Spec{TP: 1, NNodes: 1}},
		{name: "tensor parallel over two nodes", parallel: parallelism.Spec{TP: 2, NNodes: 2}, wantReject: true},
		{name: "pipeline parallel over two nodes", parallel: parallelism.Spec{TP: 1, PP: 2, NNodes: 2}, wantReject: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			out := (Input{Model: model, Node: node, Engine: "vllm", PlacementResolver: resolver, Quick: quick, Parallel: test.parallel}).Evaluate(st)
			if !test.wantReject {
				if !out.Result.OK || out.PlacementError != nil {
					t.Fatalf("refused: %+v; placement error=%v", out.Result.Message, out.PlacementError)
				}
				return
			}
			if out.Result.OK {
				t.Fatalf("admitted with plan %+v, want the resolver to refuse the rank", out.Result.Plan)
			}
			if out.Result.Message.Code != scheduler.ReasonPlacementRejected || out.Result.Message.Params["detail"] != reason {
				t.Fatalf("refusal = %+v, want %s with the resolver's reason", out.Result.Message, scheduler.ReasonPlacementRejected)
			}
		})
	}
}

// The launch assembly follows the plan: a rank the resolver refuses gets no
// command, so a single-GPU template never reaches the engine as
// --tensor-parallel-size 2 --nnodes 2 over two nodes with a card apiece.
func TestPlanLaunchAssemblesNoRankOfAGroupTheResolverRefuses(t *testing.T) {
	st, err := store.Open("file:planning_plan_launch_nnodes?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("plan-launch-nnodes", store.HashToken("token"), "plan-launch-nnodes")
	if err != nil {
		t.Fatal(err)
	}
	node.GPUs = []store.GPU{{Index: 0, MemoryTotal: 80 << 30, Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0"}}
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/single-gpu", Name: "single-gpu", Arch: "qwen3"})
	if err != nil {
		t.Fatal(err)
	}
	selection, err := deploy.DecodeCatalogSelection([]byte(`{"engine":"vllm","spec_name":"single","spec_source":"upstream","weight_bytes":8589934592,"gpus":{"vendor":"nvidia","min_count":1},"placement":{"language":"starlark","resolver":"def resolve(gpu):\n    if gpu.count != 1 or gpu.nnodes != 1:\n        return reject(\"runs on one GPU of one node\")\n    return {\"TP_SIZE\": 1}"}}`))
	if err != nil {
		t.Fatal(err)
	}
	request := func(placement Placement, topology DistTopology) LaunchRequest {
		return LaunchRequest{
			Engine: "vllm", Model: model, Node: node, Selection: selection, Placement: placement, Topology: topology,
			Source:     LaunchSourceFromDeployment(&store.Deployment{ID: 7, Image: "registry.example/app:v1"}),
			ModelMount: deploy.DefaultModelMount, ServedName: "single-gpu", APIPort: 30000,
			MasterAddr: "10.0.0.1", MasterPort: deploy.DefaultMasterPort,
		}
	}
	alone, err := PlanLaunch(context.Background(), st, storeNetworks(st), request(Placement{}, DistTopology{NNodes: 1}))
	if err != nil {
		t.Fatal(err)
	}
	if !alone.Output.Result.OK || !strings.Contains(strings.Join(alone.Resolved.Argv, " "), "--tensor-parallel-size 1") {
		t.Fatalf("one card of one node: result=%+v argv=%v", alone.Output.Result.Message, alone.Resolved.Argv)
	}
	for name, placement := range map[string]Placement{"tensor parallel 2": {TP: 2}, "pipeline parallel 2": {TP: 1, PP: 2}} {
		for rank := range 2 {
			topology := DistTopology{NNodes: 2, NodeRank: rank, Headless: rank > 0, Role: "leader", NodeAddress: "10.0.0.1"}
			plan, err := PlanLaunch(context.Background(), st, storeNetworks(st), request(placement, topology))
			if err != nil {
				t.Fatalf("%s rank %d: %v", name, rank, err)
			}
			if plan.Output.Result.OK || len(plan.Resolved.Argv) != 0 {
				t.Fatalf("%s rank %d of two nodes was assembled: %v", name, rank, plan.Resolved.Argv)
			}
			if plan.Output.Result.Message.Code != scheduler.ReasonPlacementRejected {
				t.Fatalf("%s rank %d refusal = %+v", name, rank, plan.Output.Result.Message)
			}
		}
	}
}
