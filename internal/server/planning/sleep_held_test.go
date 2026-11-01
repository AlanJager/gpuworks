package planning

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// A sharing deployment with one replica asleep by the platform's choice and
// one an operator holds asleep keeps only the former's card: a request wakes
// that one, and none wakes the held one, so its card is anyone's.
func TestOnlyTheSleepersARequestWakesKeepTheirCards(t *testing.T) {
	st, node, chat, _ := reservedNode(t, 1, false)
	if _, err := st.CreateInstance(&store.Instance{UUID: "chat-2", DeploymentID: chat.ID, NodeID: node.ID, State: "running", GPUIndexes: "[1]", NNodes: 1, DesiredStateJSON: sleepSpec("chat-2", 0)}); err != nil {
		t.Fatal(err)
	}
	if err := st.OrderInstancesSleepHeld(chat.ID, []string{"chat-2"}, 1, proto.SleepModeWait, true); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Model(&store.Instance{}).Where("uuid = ?", "chat-2").Update("sleep_level", 1).Error; err != nil {
		t.Fatal(err)
	}
	held, err := HeldGPUs(st, yieldpolicy.Never)
	if err != nil {
		t.Fatal(err)
	}
	if !held[node.ID][0] || held[node.ID][1] {
		t.Fatalf("held = %v, want card 0 (chat-1, woken by a request) and not card 1 (chat-2, held)", held[node.ID])
	}
	reserved := reservedAgainst(st, yieldpolicy.Never)
	for uuid, want := range map[string]bool{"chat-1": true, "chat-2": false} {
		in, err := st.InstanceByUUID(uuid)
		if err != nil {
			t.Fatal(err)
		}
		if got := reserved(in); got != want {
			t.Errorf("%s keeps its card = %v, want %v", uuid, got, want)
		}
	}
	// The operator wakes chat-2 and the arbiter puts it to sleep again: now
	// a request wakes it, and it keeps its card too.
	if err := st.OrderInstancesSleep(chat.ID, []string{"chat-2"}, 0, ""); err != nil {
		t.Fatal(err)
	}
	if err := st.OrderInstancesSleep(chat.ID, []string{"chat-2"}, 1, proto.SleepModeWait); err != nil {
		t.Fatal(err)
	}
	if held, err := HeldGPUs(st, yieldpolicy.Never); err != nil || !held[node.ID][0] || !held[node.ID][1] {
		t.Fatalf("held = %v err=%v, want both cards kept", held[node.ID], err)
	}
}

// A request wakes a deployment when one of its running replicas is asleep and
// not held; a held one, an awake one, a follower or one not running does not
// count.
func TestRequestWakesAsksEachReplicaAsleep(t *testing.T) {
	dep := &store.Deployment{Sleepable: true}
	asleep := func(held bool) store.Instance {
		return store.Instance{State: proto.InstRunning, SleepLevel: 1, SleepHeld: held}
	}
	follower := asleep(false)
	follower.Headless = true
	stopped := asleep(false)
	stopped.State = proto.InstStopped
	for _, tc := range []struct {
		name  string
		dep   *store.Deployment
		insts []store.Instance
		want  bool
	}{
		{"none", dep, nil, false},
		{"held only", dep, []store.Instance{asleep(true), asleep(true)}, false},
		{"a mix", dep, []store.Instance{asleep(true), asleep(false)}, true},
		{"awake beside held", dep, []store.Instance{{State: proto.InstRunning}, asleep(true)}, false},
		{"a follower or a stopped one", dep, []store.Instance{follower, stopped, asleep(true)}, false},
		{"not sleepable", &store.Deployment{}, []store.Instance{asleep(false)}, false},
	} {
		if got := RequestWakes(tc.dep, tc.insts); got != tc.want {
			t.Errorf("%s: RequestWakes = %v, want %v", tc.name, got, tc.want)
		}
	}
}
