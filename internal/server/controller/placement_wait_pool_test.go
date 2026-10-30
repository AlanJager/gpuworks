package controller

import (
	"context"
	"fmt"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/servicelevel"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A pool replica waiting for the holders of one node's card to yield is
// rechecked on that node alone: another node freeing up does not move it
// while the waited node still fits once the holders sleep. When that node no
// longer fits even then (its holder stops sharing), the search goes on down
// the pool.
func TestARecheckOfAPlacementWaitTriesTheWaitedNodeAlone(t *testing.T) {
	st, hub, nodes, model := poolFixture(t, "pool_wait_recheck", 3, 1)
	policy := func(yield string) servicelevel.Policy {
		p, _ := servicelevel.Parse(yield, servicelevel.Overrides{})
		return p
	}
	batch, err := st.CreateDeployment(&store.Deployment{Name: "batch", ModelID: model.ID, NodeID: nodes[0].ID, ServedName: "batch", Engine: "vllm", Sleepable: true, State: store.DeployActive, Replicas: 3})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetDeploymentYieldPolicy(batch.ID, policy(servicelevel.Anytime)); err != nil {
		t.Fatal(err)
	}
	for i, node := range nodes {
		if _, err := st.CreateInstance(&store.Instance{UUID: fmt.Sprintf("batch-%d", i), DeploymentID: batch.ID, NodeID: node.ID, State: proto.InstRunning, GPUIndexes: "[0]"}); err != nil {
			t.Fatal(err)
		}
	}
	online := poolDeployment(t, st, model, nodes[0].ID, 1, store.PlacementSpread, nil)
	if err := st.SetDeploymentYieldPolicy(online.ID, policy(servicelevel.Idle)); err != nil {
		t.Fatal(err)
	}
	if online, err = st.GetDeployment(online.ID); err != nil {
		t.Fatal(err)
	}
	c := New(st, hub, nil)
	pick := func() (*store.Node, bool) {
		t.Helper()
		return c.pickPoolNode(context.Background(), &unitDeployment{dep: online}, model)
	}

	// Every node fits once batch sleeps there: the replica waits for one.
	if node, ok := pick(); ok {
		t.Fatalf("placed on %s with batch awake everywhere", node.UUID)
	}
	w, waiting := c.PlacementWaits().Get(online.ID, c.now())
	if !waiting {
		t.Fatal("no placement wait noted")
	}
	var waited, other *store.Node
	for _, node := range nodes {
		switch {
		case node.ID == w.Cards.NodeID:
			waited = node
		case other == nil:
			other = node
		}
	}
	if waited == nil {
		t.Fatalf("the wait names node %d, none of the pool's", w.Cards.NodeID)
	}

	// Another node frees up; the recheck tries the waited node alone, which
	// still fits once batch sleeps there, and goes on waiting for it.
	if err := st.DeleteInstance(instanceOn(t, st, batch.ID, other.ID)); err != nil {
		t.Fatal(err)
	}
	if node, ok := pick(); ok {
		t.Fatalf("a recheck placed on %s: it planned past the node it waits for", node.UUID)
	}
	if w, waiting := c.PlacementWaits().Get(online.ID, c.now()); !waiting || w.Cards.NodeID != waited.ID {
		t.Fatalf("wait after the recheck = %+v %v, want it still on node %d", w, waiting, waited.ID)
	}

	// Batch stops sharing its cards: the waited node no longer fits even
	// with a yield, and the search goes on to the free node.
	if err := st.SetDeploymentYieldPolicy(batch.ID, policy(servicelevel.Never)); err != nil {
		t.Fatal(err)
	}
	node, ok := pick()
	if !ok || node.ID != other.ID {
		t.Fatalf("picked %v ok=%v, want the free node %s", node, ok, other.UUID)
	}
}

func instanceOn(t *testing.T, st *store.Store, depID, nodeID uint) string {
	t.Helper()
	insts, err := st.InstancesForDeployment(depID)
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range insts {
		if in.NodeID == nodeID {
			return in.UUID
		}
	}
	t.Fatalf("deployment %d has no instance on node %d", depID, nodeID)
	return ""
}
