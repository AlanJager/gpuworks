package store

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

// Draining is the server's: the agent keeps reporting the engine as running
// (or crashed) while it finishes its requests, and none of that may put the
// instance back in its replica or count it as a failed attempt. Only the
// report that it is gone lands.
func TestDrainingSurvivesAgentReportsUntilStopped(t *testing.T) {
	st, err := Open("file:store_instance_draining_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("draining-node", HashToken("draining-token"), "draining")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&Deployment{ModelID: 1, NodeID: node.ID, State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&Instance{UUID: "draining-inst", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetInstanceState("draining-inst", proto.InstDraining, "restart requested"); err != nil {
		t.Fatal(err)
	}
	for _, report := range []proto.InstanceStatus{
		{UUID: "draining-inst", State: proto.InstRunning},
		{UUID: "draining-inst", State: proto.InstFailed, Message: "engine exited"},
		{UUID: "draining-inst", State: proto.InstStarting},
	} {
		if err := st.UpdateInstanceStatusFromNode(node.ID, report); err != nil {
			t.Fatal(err)
		}
		got, err := st.InstanceByUUID("draining-inst")
		if err != nil {
			t.Fatal(err)
		}
		if got.State != proto.InstDraining {
			t.Fatalf("a %s report replaced draining: %+v", report.State, got)
		}
	}
	if err := st.UpdateInstanceStatusFromNode(node.ID, proto.InstanceStatus{UUID: "draining-inst", State: proto.InstStopped}); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.InstanceByUUID("draining-inst"); got.State != proto.InstStopped {
		t.Fatalf("the stopped report did not land: %s", got.State)
	}
	// A draining instance is still desired: a reconnecting agent must not
	// tear it down before it has drained.
	if err := st.SetInstanceState("draining-inst", proto.InstDraining, "again"); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Model(&Instance{}).Where("uuid = ?", "draining-inst").Update("spec", `{"uuid":"draining-inst","desired_state":"running"}`).Error; err != nil {
		t.Fatal(err)
	}
	desired, err := st.DesiredStateForNode(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(desired.Instances) != 1 || desired.Instances[0].UUID != "draining-inst" || desired.Instances[0].DesiredState == "stopped" {
		t.Fatalf("desired state for a draining instance = %+v", desired.Instances)
	}
}
