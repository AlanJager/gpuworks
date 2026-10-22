package controller

import (
	"fmt"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A replacement generation led from a node waits while a leaving leader of
// the same ordinary multi-node deployment may still listen on the master port
// there; nothing else holds it back.
func TestMasterPortHolderIsALeavingLeaderOnTheSameNode(t *testing.T) {
	leaderNode := &store.Node{ID: 1}
	multi := &store.Deployment{ID: 1, NNodes: 2}
	row := func(uuid, state string, node uint, headless bool, nnodes int) store.Instance {
		return store.Instance{UUID: uuid, State: state, NodeID: node, Headless: headless, NNodes: nnodes}
	}
	for _, tc := range []struct {
		name string
		dep  *store.Deployment
		rows []store.Instance
		want string
	}{
		{"a draining leader on the node", multi, []store.Instance{row("old", proto.InstDraining, 1, false, 2)}, "old"},
		{"a removing leader on the node", multi, []store.Instance{row("old", proto.InstRemoving, 1, false, 2)}, "old"},
		{"a leader already stopped", multi, []store.Instance{row("old", proto.InstStopped, 1, false, 2)}, ""},
		{"a draining follower on the node", multi, []store.Instance{row("old", proto.InstDraining, 1, true, 2)}, ""},
		{"a draining leader on another node", multi, []store.Instance{row("old", proto.InstDraining, 2, false, 2)}, ""},
		{"a single-node deployment", &store.Deployment{ID: 2, NNodes: 1}, []store.Instance{row("old", proto.InstDraining, 1, false, 1)}, ""},
	} {
		got := masterPortHolder(tc.dep, tc.rows, leaderNode)
		if (got == nil && tc.want != "") || (got != nil && got.UUID != tc.want) {
			t.Errorf("%s: holder = %+v, want %q", tc.name, got, tc.want)
		}
	}
}

// A deleted node leaves the deployment's node set; a failed read fails the
// pass instead of handing the matcher a partial set that would read healthy
// replicas as unclaimed.
func TestDeploymentNodesFailsTheReadRatherThanDroppingANode(t *testing.T) {
	st, err := store.Open("file:controller_deployment_nodes_"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	a, _ := st.RegisterNode("dn-a", store.HashToken("a"), "dn-a")
	b, _ := st.RegisterNode("dn-b", store.HashToken("b"), "dn-b")
	c := New(st, &distributedTestHub{online: map[string]bool{}, updates: map[string][]proto.StateUpdate{}}, nil)
	dep := &store.Deployment{ID: 1, NodeID: a.ID, NodeIDs: fmt.Sprintf("[%d,%d,%d]", a.ID, b.ID, 999)}
	nodes, err := c.deploymentNodes(dep)
	if err != nil || len(nodes) != 2 || nodes[0].UUID != "dn-a" || nodes[1].UUID != "dn-b" {
		t.Fatalf("nodes = %+v err=%v, want dn-a, dn-b with the deleted node left out", nodes, err)
	}
	_ = st.Close()
	if nodes, err := c.deploymentNodes(dep); err == nil {
		t.Fatalf("a failed read returned %d nodes and no error", len(nodes))
	}
}
