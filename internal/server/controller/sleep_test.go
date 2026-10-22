package controller

import (
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"sort"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A push that fails is retried: nothing records it as delivered, and the
// deployment is requeued.
func TestSyncSleepPushFailureRequeuesAndRetries(t *testing.T) {
	st, err := store.Open("file:controller_sleep_failure_"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("sleep-node-failure", store.HashToken("token"), "sleep-node-failure")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := createTestDeployment(t, st, &store.Deployment{ModelID: 1, NodeID: node.ID, ServedName: "sleep-model", State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(proto.InstanceDesiredState{UUID: "sleep-inst-failure", DesiredState: "running", Sleepable: true, DesiredSleep: 0, APIPort: 8000})
	inst, err := st.CreateInstance(&store.Instance{UUID: "sleep-inst-failure", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, DesiredStateJSON: string(raw)})
	if err != nil {
		t.Fatal(err)
	}
	hub := &distributedTestHub{online: map[string]bool{node.UUID: true}, updates: map[string][]proto.StateUpdate{}, pushErr: errors.New("agent offline")}
	c := New(st, hub, nil)
	c.SetRetryPolicy(5*time.Millisecond, 3)
	if err := st.OrderDeploymentSleep(dep.ID, 1, proto.SleepModeWait); err != nil {
		t.Fatal(err)
	}
	c.syncSleep(dep, instancesOf(t, st, dep.ID))
	select {
	case key := <-nextWorkKey(c.q):
		if key != depKey(dep.ID) {
			t.Fatalf("retry key = %q, want %q", key, depKey(dep.ID))
		}
	case <-time.After(200 * time.Millisecond):
		t.Fatal("failed sleep push did not requeue deployment")
	}
	c.q.close()
	hub.pushErr = nil
	c.syncSleep(dep, instancesOf(t, st, dep.ID))
	pushed := hub.updates[node.UUID]
	if len(pushed) != 1 || len(pushed[0].Instances) != 1 || pushed[0].Instances[0].UUID != inst.UUID || pushed[0].Instances[0].DesiredSleep != 1 {
		t.Fatalf("retried push = %+v, want the sleep order", pushed)
	}
}

// instancesOf reads a deployment's instances as the reconcile loop does.
func instancesOf(t *testing.T, st *store.Store, depID uint) []store.Instance {
	t.Helper()
	insts, err := st.InstancesForDeployment(depID)
	if err != nil {
		t.Fatal(err)
	}
	return insts
}

func nextWorkKey(q *workq) <-chan string {
	out := make(chan string, 1)
	go func() {
		key, ok := q.next()
		if ok {
			out <- key
		}
	}()
	return out
}

// A deployment-wide order reaches every live instance and no other; an order
// for one instance reaches only that one, and the deployment's other replica
// stays awake.
func TestSyncSleepPushesEachInstancesOwnIntent(t *testing.T) {
	st, err := store.Open("file:controller_sleep_success_"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("sleep-node-success", store.HashToken("token"), "sleep-node-success")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := createTestDeployment(t, st, &store.Deployment{ModelID: 1, NodeID: node.ID, ServedName: "sleep-model", State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	states := []struct {
		uuid  string
		state string
	}{
		{uuid: "sleep-running", state: proto.InstRunning},
		{uuid: "sleep-other", state: proto.InstRunning},
		{uuid: "sleep-failed", state: proto.InstFailed},
		{uuid: "sleep-stopped", state: proto.InstStopped},
		{uuid: "sleep-pending-removal", state: proto.InstPendingRemoval},
		{uuid: "sleep-draining", state: proto.InstDraining},
	}
	for _, tc := range states {
		raw, _ := json.Marshal(proto.InstanceDesiredState{UUID: tc.uuid, DesiredState: "running", Sleepable: true, DesiredSleep: 0, APIPort: 8000})
		if _, err := st.CreateInstance(&store.Instance{UUID: tc.uuid, DeploymentID: dep.ID, NodeID: node.ID, State: tc.state, DesiredStateJSON: string(raw)}); err != nil {
			t.Fatal(err)
		}
	}
	hub := &distributedTestHub{online: map[string]bool{node.UUID: true}, updates: map[string][]proto.StateUpdate{}}
	c := New(st, hub, nil)
	c.syncSleep(dep, instancesOf(t, st, dep.ID))
	if len(hub.updates) != 0 {
		t.Fatalf("pushed before any order: %+v", hub.updates)
	}
	pushedUUIDs := func() []string {
		var out []string
		for _, update := range hub.updates[node.UUID] {
			for _, spec := range update.Instances {
				out = append(out, fmt.Sprintf("%s=%d", spec.UUID, spec.DesiredSleep))
			}
		}
		sort.Strings(out)
		hub.updates = map[string][]proto.StateUpdate{}
		return out
	}
	if err := st.OrderInstancesSleep(dep.ID, []string{"sleep-running"}, 2, proto.SleepModeWait); err != nil {
		t.Fatal(err)
	}
	c.syncSleep(dep, instancesOf(t, st, dep.ID))
	if got := pushedUUIDs(); !slices.Equal(got, []string{"sleep-running=2"}) {
		t.Fatalf("instance order pushed %v, want only sleep-running=2", got)
	}
	c.syncSleep(dep, instancesOf(t, st, dep.ID))
	if got := pushedUUIDs(); len(got) != 0 {
		t.Fatalf("delivered order pushed again: %v", got)
	}
	if err := st.OrderDeploymentSleep(dep.ID, 1, proto.SleepModeWait); err != nil {
		t.Fatal(err)
	}
	c.syncSleep(dep, instancesOf(t, st, dep.ID))
	if got := pushedUUIDs(); !slices.Equal(got, []string{"sleep-other=1", "sleep-running=1"}) {
		t.Fatalf("deployment order pushed %v, want both live replicas at 1", got)
	}
}

// The pause mode the API stamped into the instance's desired state travels with
// the level push: the agent needs both in one order.
func TestSyncSleepCarriesTheStampedPauseMode(t *testing.T) {
	st, err := store.Open("file:controller_sleep_mode_"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("sleep-node-mode", store.HashToken("token"), "sleep-node-mode")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := createTestDeployment(t, st, &store.Deployment{ModelID: 1, NodeID: node.ID, ServedName: "sleep-model", State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(proto.InstanceDesiredState{UUID: "sleep-inst-mode", DesiredState: "running", Sleepable: true, DesiredSleep: 0, APIPort: 8000})
	if _, err := st.CreateInstance(&store.Instance{UUID: "sleep-inst-mode", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, DesiredStateJSON: string(raw)}); err != nil {
		t.Fatal(err)
	}
	hub := &distributedTestHub{online: map[string]bool{node.UUID: true}, updates: map[string][]proto.StateUpdate{}}
	c := New(st, hub, nil)
	if err := st.OrderDeploymentSleep(dep.ID, 1, proto.SleepModeAbort); err != nil {
		t.Fatal(err)
	}
	c.syncSleep(dep, instancesOf(t, st, dep.ID))
	pushed := hub.updates[node.UUID]
	if len(pushed) != 1 || len(pushed[0].Instances) != 1 {
		t.Fatalf("pushes = %+v", pushed)
	}
	if got := pushed[0].Instances[0]; got.DesiredSleep != 1 || got.SleepMode != "abort" {
		t.Fatalf("pushed spec sleep=%d mode=%q, want 1/abort", got.DesiredSleep, got.SleepMode)
	}
}

// An instance the controller has not pushed to in this process (after a
// server restart the agent's reconnect carried its desired state) is pushed
// only when its reported level differs from its intent, and then once.
func TestSyncSleepPushesAnUnseenInstanceOnlyWhenItsLevelDiffers(t *testing.T) {
	st, err := store.Open("file:controller_sleep_observed_"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("sleep-node-observed", store.HashToken("token"), "sleep-node-observed")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := createTestDeployment(t, st, &store.Deployment{ModelID: 1, NodeID: node.ID, ServedName: "sleep-model", State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		uuid          string
		intent, level int
	}{{"sleep-settled", 1, 1}, {"sleep-behind", 1, 0}} {
		// Instances start under the deployment's standing order (awake);
		// the order for these replicas comes after, as an operator's would.
		raw, _ := json.Marshal(proto.InstanceDesiredState{UUID: tc.uuid, DesiredState: "running", Sleepable: true, APIPort: 8000})
		if _, err := st.CreateInstance(&store.Instance{UUID: tc.uuid, DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, SleepLevel: tc.level, DesiredStateJSON: string(raw)}); err != nil {
			t.Fatal(err)
		}
		if err := st.OrderInstancesSleep(dep.ID, []string{tc.uuid}, tc.intent, proto.SleepModeWait); err != nil {
			t.Fatal(err)
		}
	}
	hub := &distributedTestHub{online: map[string]bool{node.UUID: true}, updates: map[string][]proto.StateUpdate{}}
	c := New(st, hub, nil)
	c.syncSleep(dep, instancesOf(t, st, dep.ID))
	pushed := hub.updates[node.UUID]
	if len(pushed) != 1 || len(pushed[0].Instances) != 1 || pushed[0].Instances[0].UUID != "sleep-behind" {
		t.Fatalf("first sync pushed %+v, want only sleep-behind", pushed)
	}
	hub.updates = map[string][]proto.StateUpdate{}
	c.syncSleep(dep, instancesOf(t, st, dep.ID))
	if len(hub.updates) != 0 {
		t.Fatalf("second sync pushed again: %+v", hub.updates)
	}
}

// A launch push records the intent it carried, so an order written between
// the instance's creation and that push is pushed on the next sync.
func TestSyncSleepPushesAnOrderThatRacedTheLaunchPush(t *testing.T) {
	st, err := store.Open("file:controller_sleep_launch_race_"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("sleep-node-race", store.HashToken("token"), "sleep-node-race")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := createTestDeployment(t, st, &store.Deployment{ModelID: 1, NodeID: node.ID, ServedName: "sleep-model", State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	launched := proto.InstanceDesiredState{UUID: "sleep-raced", DesiredState: "running", Sleepable: true, APIPort: 8000}
	raw, _ := json.Marshal(launched)
	if _, err := st.CreateInstance(&store.Instance{UUID: launched.UUID, DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstPending, DesiredStateJSON: string(raw)}); err != nil {
		t.Fatal(err)
	}
	if err := st.OrderInstancesSleep(dep.ID, []string{launched.UUID}, 1, proto.SleepModeWait); err != nil {
		t.Fatal(err)
	}
	hub := &distributedTestHub{online: map[string]bool{node.UUID: true}, updates: map[string][]proto.StateUpdate{}}
	c := New(st, hub, nil)
	c.recordSleepPushed(dep.ID, launched)
	c.syncSleep(dep, instancesOf(t, st, dep.ID))
	pushed := hub.updates[node.UUID]
	if len(pushed) != 1 || pushed[0].Instances[0].DesiredSleep != 1 {
		t.Fatalf("raced order pushed %+v, want level 1", pushed)
	}
}
