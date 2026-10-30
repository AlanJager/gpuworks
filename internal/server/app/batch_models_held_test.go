package app

import (
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/batch"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// An operator's sleep of one replica holds that replica (Instance.SleepHeld),
// but the replica left awake still serves: the name
// is served as long as one replica is awake with no sleep ordered, and held
// once none is.
func TestBatchModelsAHeldReplicaLeavesTheOthersServing(t *testing.T) {
	st, _ := modelStack(t, &scriptedSender{reply: func(string, []byte) batch.Reply { return batch.Reply{Status: 200} }})
	node, err := st.RegisterNode("n1", store.HashToken("token"), "n1")
	if err != nil {
		t.Fatal(err)
	}
	d, err := st.CreateDeployment(&store.Deployment{Name: "chat", ServedName: "chat", State: store.DeployActive, Sleepable: true, Replicas: 2})
	if err != nil {
		t.Fatal(err)
	}
	for _, uuid := range []string{"chat-1", "chat-2"} {
		spec, _ := json.Marshal(proto.InstanceDesiredState{UUID: uuid, DesiredState: "running", Sleepable: true})
		if _, err := st.CreateInstance(&store.Instance{UUID: uuid, DeploymentID: d.ID, NodeID: node.ID, State: proto.InstRunning, GPUIndexes: "[0]", NNodes: 1, DesiredStateJSON: string(spec)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.OrderInstancesSleepHeld(d.ID, []string{"chat-1"}, 1, proto.SleepModeWait, true); err != nil {
		t.Fatal(err)
	}
	m := batchModels{st}
	check := func(want batch.ModelState) {
		t.Helper()
		got, err := m.Lookup("chat")
		if err != nil || got != want {
			t.Fatalf("Lookup = %v err=%v, want %v", got, err, want)
		}
		list, err := m.List()
		if err != nil || len(list) != 1 || list[0].State != want {
			t.Fatalf("List = %v err=%v, want %v", list, err, want)
		}
	}
	check(batch.ModelAvailable)
	// The other replica is ordered to sleep too: the name is held, before
	// either engine reports the sleep.
	if err := st.OrderInstancesSleepHeld(d.ID, []string{"chat-2"}, 1, proto.SleepModeWait, true); err != nil {
		t.Fatal(err)
	}
	check(batch.ModelSleepHeld)
}
