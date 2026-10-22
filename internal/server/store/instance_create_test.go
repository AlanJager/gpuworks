package store

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

// A replica generation is written whole or not at all: a rank that fails to
// insert takes the ranks before it with it, and nothing is announced. A clean
// write announces every rank after the commit.
func TestCreateInstancesWritesAGenerationWholeOrNotAtAll(t *testing.T) {
	st, err := Open("file:store_create_instances_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("gang-node", HashToken("gang-token"), "gang")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&Deployment{ModelID: 1, NodeID: node.ID, State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&Instance{UUID: "taken", DeploymentID: dep.ID, NodeID: node.ID}); err != nil {
		t.Fatal(err)
	}
	sub := st.Bus.Subscribe(16, "instance")
	t.Cleanup(sub.Close)

	reads := [][]InstanceModelFile{{{ModelID: 1, ModelFileID: 7, HostPath: "/opt/gpuworks/models/m"}}, {{ModelID: 1, ModelFileID: 7, HostPath: "/opt/gpuworks/models/m"}}}
	err = st.CreateInstances([]*Instance{
		{UUID: "leader", DeploymentID: dep.ID, NodeID: node.ID},
		{UUID: "taken", DeploymentID: dep.ID, NodeID: node.ID},
	}, reads)
	if err == nil {
		t.Fatal("a generation whose follower collides was written")
	}
	var leftover int64
	if err := st.DB().Model(&InstanceModelFile{}).Count(&leftover).Error; err != nil || leftover != 0 {
		t.Fatalf("model file records of the refused generation = %d err=%v, want rolled back", leftover, err)
	}
	if _, err := st.InstanceByUUID("leader"); err != ErrNotFound {
		t.Fatalf("leader of the refused generation = %v, want rolled back", err)
	}
	events, err := st.DeploymentEvents(dep.ID, 100)
	if err != nil {
		t.Fatal(err)
	}
	for _, ev := range events {
		if ev.InstanceUUID == "leader" {
			t.Fatalf("the refused leader left a timeline row: %+v", ev)
		}
	}
	select {
	case ev := <-sub.C():
		t.Fatalf("a refused generation was announced: %+v", ev)
	default:
	}

	if err := st.CreateInstances([]*Instance{
		{UUID: "leader", DeploymentID: dep.ID, NodeID: node.ID},
		{UUID: "follower", DeploymentID: dep.ID, NodeID: node.ID},
	}, reads); err != nil {
		t.Fatal(err)
	}
	for _, uuid := range []string{"leader", "follower"} {
		if inst, err := st.InstanceByUUID(uuid); err != nil || inst.State == "" {
			t.Fatalf("%s = %+v, %v", uuid, inst, err)
		}
		select {
		case ev := <-sub.C():
			if ev.Key != uuid {
				t.Fatalf("announced %q, want %q", ev.Key, uuid)
			}
		default:
			t.Fatalf("%s was not announced", uuid)
		}
	}
	leader, _ := st.InstanceByUUID("leader")
	recorded, err := st.InstanceModelFiles([]uint{leader.ID})
	if err != nil || len(recorded[leader.ID]) != 1 || recorded[leader.ID][0].HostPath != "/opt/gpuworks/models/m" || recorded[leader.ID][0].ModelFileID != 7 {
		t.Fatalf("leader reads = %+v err=%v", recorded, err)
	}
	// Deleting the instance takes its records with it.
	if err := st.DeleteInstance("leader"); err != nil {
		t.Fatal(err)
	}
	if recorded, _ = st.InstanceModelFiles([]uint{leader.ID}); len(recorded[leader.ID]) != 0 {
		t.Fatalf("records outlived their instance: %+v", recorded)
	}
}
