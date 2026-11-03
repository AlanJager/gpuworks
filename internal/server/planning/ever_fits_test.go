package planning

import (
	"context"
	"fmt"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
	"gpuworks.ai/gpuworks/internal/server/store"
	"testing"
)

// A pool is judged one node per hardware shape: six small nodes of two card
// models cost two plans and are refused with the first shape's reason; one
// big node among them fits. Nodes the requirements exclude are not counted.
func TestEverFitsPlansOneNodePerHardwareShape(t *testing.T) {
	st, err := store.Open("file:planning_ever_fits?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/ever-fits", Name: "ever-fits", WeightBytes: 20 << 30})
	if err != nil {
		t.Fatal(err)
	}
	var nodes []*store.Node
	add := func(name, card string, memory int64, labels string) {
		node, err := st.RegisterNode(name, store.HashToken(name), name)
		if err != nil {
			t.Fatal(err)
		}
		node.GPUVendor, node.Arch, node.Labels = "nvidia", "amd64", labels
		node.GPUs = []store.GPU{{Index: 0, Name: card, MemoryTotal: memory, Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0"}}
		nodes = append(nodes, node)
	}
	for i := 0; i < 4; i++ {
		add(fmt.Sprintf("small-a-%d", i), "NVIDIA A10", 16<<30, `{"pool":"p"}`)
	}
	for i := 0; i < 2; i++ {
		add(fmt.Sprintf("small-b-%d", i), "NVIDIA A16", 16<<30, `{"pool":"p"}`)
	}
	add("big-outside", "NVIDIA H100", 80<<30, `{"pool":"other"}`)
	reqs := nodelabel.Requirements{"pool": {"p"}}
	built := 0
	build := func(node *store.Node) (LaunchRequest, deploy.CatalogSelection, error) {
		built++
		return LaunchRequest{
			Engine: "vllm", Model: model, Node: node, Topology: DistTopology{NNodes: 1, Role: "standalone"},
			Source: LaunchSourceFromDeployment(&store.Deployment{Image: "registry.example/vllm:v1"}), ServedName: "ever-fits",
		}, deploy.CatalogSelection{}, nil
	}
	never, err := EverFits(context.Background(), st, storeNetworks(st), nodes, NodeFacts{}, reqs, build)
	if err != nil {
		t.Fatal(err)
	}
	if never == nil || never.Pool != 6 || never.Shapes != 2 || never.Node != "small-a-0" || never.Reason.Code == "" || built != 2 {
		t.Fatalf("never = %+v built %d, want pool 6, 2 shapes, refused on small-a-0 after 2 plans", never, built)
	}

	// Refused for memory, not for anything about the fixture.
	if never.Reason.Code != "no_fit" {
		t.Fatalf("refusal = %+v, want no_fit", never.Reason)
	}
	add("big-inside", "NVIDIA H100", 80<<30, `{"pool":"p"}`)
	built = 0
	if never, err := EverFits(context.Background(), st, storeNetworks(st), nodes, NodeFacts{}, reqs, build); err != nil || never != nil || built != 3 {
		t.Fatalf("with a big node in the pool: never = %+v err = %v built %d", never, err, built)
	}

	never, err = EverFits(context.Background(), st, storeNetworks(st), nodes, NodeFacts{}, nodelabel.Requirements{"pool": {"nowhere"}}, build)
	if err != nil || never == nil || never.Pool != 0 || never.Shapes != 0 || never.Node != "" {
		t.Fatalf("an empty pool: never = %+v err = %v", never, err)
	}
}
