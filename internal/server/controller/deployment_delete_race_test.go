package controller

import (
	"context"
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestDeletingDeploymentSurvivesStopUntilTeardownCompletes(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("deleting-node", store.HashToken("token"), "deleting-node")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&store.Deployment{NodeID: node.ID, State: store.DeployActive})
	if err != nil {
		t.Fatal(err)
	}
	inst, err := st.CreateInstance(&store.Instance{UUID: "deleting-instance", NodeID: node.ID, DeploymentID: dep.ID, State: proto.InstRunning})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetDeploymentState(dep.ID, store.DeployDeleting); err != nil {
		t.Fatal(err)
	}
	if err := st.SetDeploymentState(dep.ID, store.DeployStopped); !errors.Is(err, store.ErrStateChanged) {
		t.Errorf("stop after deletion = %v, want ErrStateChanged", err)
	}
	c := New(st, &distributedTestHub{updates: map[string][]proto.StateUpdate{}}, nil)
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	if current, err := st.GetDeployment(dep.ID); err != nil || current.State != store.DeployDeleting {
		t.Fatalf("before teardown acknowledgement: deployment=%+v err=%v", current, err)
	}
	if current, err := st.InstanceByUUID(inst.UUID); err != nil || current.State != proto.InstRemoving {
		t.Fatalf("before teardown acknowledgement: instance=%+v err=%v", current, err)
	}
	if err := st.SetInstanceState(inst.UUID, proto.InstStopped, "agent confirmed teardown"); err != nil {
		t.Fatal(err)
	}
	if err := c.reconcileDeployment(context.Background(), dep.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetDeployment(dep.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("deployment after teardown = %v, want ErrNotFound", err)
	}
	if _, err := st.InstanceByUUID(inst.UUID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("instance after teardown = %v, want ErrNotFound", err)
	}
}
