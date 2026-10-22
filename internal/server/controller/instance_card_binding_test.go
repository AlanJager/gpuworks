package controller

import (
	"context"
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A launch records which physical cards it was placed on, not only the
// device indexes they happen to carry: an index numbers only the cards the
// driver could initialize, so it moves when that set changes and would point
// the instance at another card on its next start.
func TestScheduledInstanceBindsTheCardsItWasPlacedOn(t *testing.T) {
	st, err := store.Open("file:controller_card_binding?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("card-binding-node", store.HashToken("t"), "card-binding-node")
	if err != nil {
		t.Fatal(err)
	}
	for _, card := range []struct {
		index int
		bdf   string
	}{{0, "0000:1a:00.0"}, {1, "0000:3f:00.0"}} {
		if err := st.DB().Create(&store.GPU{
			NodeID: node.ID, Index: card.index, UUID: "GPU-" + card.bdf, PCIeBDF: card.bdf,
			Name: "NVIDIA A100-SXM4-80GB", Vendor: "nvidia", ArchID: "sm_80",
			ComputeCapability: "8.0", MemoryTotal: 80 << 30,
		}).Error; err != nil {
			t.Fatal(err)
		}
	}
	node, err = st.NodeWithGPUs(node.UUID)
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "fixture/card-binding", Revision: "main", Name: "card-binding",
		WeightBytes: 1 << 30,
		Meta:        `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/card-binding"}); err != nil {
		t.Fatal(err)
	}
	dep, err := createTestDeployment(t, st, &store.Deployment{
		Name: "card-binding", ModelID: model.ID, NodeID: node.ID, ServedName: "card-binding", Engine: "vllm",
		State: "active", Placement: `{"tp":1,"gpus":[1]}`, QuickParams: `{"max_model_len":512}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	hub := &distributedTestHub{online: map[string]bool{node.UUID: true}, updates: map[string][]proto.StateUpdate{}}
	if err := New(st, hub, nil).reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatalf("reconcile: %v", err)
	}

	updates := hub.updates[node.UUID]
	if len(updates) != 1 || len(updates[0].Instances) != 1 {
		t.Fatalf("state updates = %+v, want one instance push", updates)
	}
	spec := updates[0].Instances[0]
	if len(spec.GPUIdents) != 1 || spec.GPUIdents[0] != "pci:0000:3f:00.0" {
		t.Fatalf("pushed idents = %v, want the identity of the pinned card at device 1", spec.GPUIdents)
	}
	if len(spec.GPUIndexes) != 1 || spec.GPUIndexes[0] != 1 {
		t.Fatalf("pushed indexes = %v, want [1] alongside the identity", spec.GPUIndexes)
	}

	instances, err := st.InstancesForDeployment(dep.ID)
	if err != nil || len(instances) != 1 {
		t.Fatalf("instances = %+v err = %v", instances, err)
	}
	var persisted []string
	if err := json.Unmarshal([]byte(instances[0].GPUIdents), &persisted); err != nil {
		t.Fatalf("persisted idents %q: %v", instances[0].GPUIdents, err)
	}
	if len(persisted) != 1 || persisted[0] != "pci:0000:3f:00.0" {
		t.Fatalf("persisted idents = %v", persisted)
	}
}

// A node whose prober reports neither a PCI address nor a UUID (test
// fixtures, and probers that know only indexes) is placed as before, with no
// identity at all rather than a partial one.
func TestScheduledInstanceLeavesUnidentifiedCardsUnbound(t *testing.T) {
	node := &store.Node{GPUs: []store.GPU{{Index: 0}, {Index: 1}}}
	if got := store.CardIdentsFor(node, []int{0}); got != nil {
		t.Fatalf("idents on an unidentified node = %v, want none", got)
	}
}
