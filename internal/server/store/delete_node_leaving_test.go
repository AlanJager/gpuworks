package store

import "testing"

// A node whose only rows wait for a removal it can no longer confirm (a
// replica moved away while it was offline) can be deleted, taking those rows
// with it; a node still running an instance cannot.
func TestDeleteNodeTakesRowsOnlyWaitingToBeRemoved(t *testing.T) {
	st := newTestStore(t)
	node, err := st.RegisterNode("gone-node", HashToken("t"), "gone-node")
	if err != nil {
		t.Fatal(err)
	}
	dep, _ := st.CreateDeployment(&Deployment{Name: "d", ServedName: "d", State: DeployActive})
	if _, err := st.CreateInstance(&Instance{UUID: "moved-away", DeploymentID: dep.ID, NodeID: node.ID, State: "removing"}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&Instance{UUID: "still-running", DeploymentID: dep.ID, NodeID: node.ID, State: "running"}); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteNode(node.UUID); err == nil {
		t.Fatal("a node running an instance was deleted")
	}
	if err := st.DeleteInstance("still-running"); err != nil {
		t.Fatal(err)
	}
	if err := st.DeleteNode(node.UUID); err != nil {
		t.Fatalf("delete a node with only a row waiting to be removed: %v", err)
	}
	if _, err := st.InstanceByUUID("moved-away"); err == nil {
		t.Fatal("the waiting row outlived its node")
	}
}
