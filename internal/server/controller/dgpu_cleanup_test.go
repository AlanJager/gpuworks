package controller

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestDeletingDeploymentWaitsForExplicitTeardownAck(t *testing.T) {
	for _, state := range []string{proto.InstRunning, proto.InstRemoving, proto.InstFailed, proto.InstPendingRemoval} {
		if allInstancesTerminal([]store.Instance{{State: state}}) {
			t.Fatalf("%s was treated as proof of container removal", state)
		}
	}
	if !allInstancesTerminal([]store.Instance{{State: proto.InstStopped}}) {
		t.Fatal("stopped should confirm teardown")
	}
}

func TestStoppedDeploymentReplaysTeardownAfterPushFailure(t *testing.T) {
	st, err := store.Open("file:controller_stopped_replay?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("stopped-replay-node", store.HashToken("stopped-replay-token"), "stopped-replay")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&store.Deployment{NodeID: node.ID, State: store.DeployStopped})
	if err != nil {
		t.Fatal(err)
	}
	spec, _ := json.Marshal(proto.InstanceDesiredState{UUID: "failed-but-may-have-container", DesiredState: "running"})
	for _, inst := range []store.Instance{
		{UUID: "failed-but-may-have-container", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstFailed, DesiredStateJSON: string(spec)},
		{UUID: "pending-without-spec", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstPendingRemoval},
	} {
		if _, err := st.CreateInstance(&inst); err != nil {
			t.Fatal(err)
		}
	}
	hub := &distributedTestHub{pushErr: errors.New("agent offline")}
	c := New(st, hub, nil)
	if err := c.reconcileDeployment(context.Background(), dep.ID); err == nil {
		t.Fatal("expected push failure")
	}
	desired, err := st.DesiredStateForNode(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(desired.Instances) != 2 {
		t.Fatalf("cleanup replay lost instances: %+v", desired.Instances)
	}
	for _, got := range desired.Instances {
		if got.DesiredState != "stopped" {
			t.Fatalf("cleanup replay is not stopped: %+v", got)
		}
	}
}
