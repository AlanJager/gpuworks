package store

import (
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestOrderDeploymentSleepUpdatesLevelAndModeTogether(t *testing.T) {
	st := newTestStore(t)
	defer st.Close()
	node, err := st.RegisterNode("sleep-order-node", HashToken("token"), "sleep-order-node")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&Deployment{ModelID: 1, NodeID: node.ID, Name: "sleep-order", ServedName: "sleep-order", Engine: "vllm", Sleepable: true, State: DeployActive})
	if err != nil {
		t.Fatal(err)
	}
	raw, _ := json.Marshal(proto.InstanceDesiredState{UUID: "sleep-order-inst", DesiredState: "running", Sleepable: true})
	if _, err := st.CreateInstance(&Instance{UUID: "sleep-order-inst", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, DesiredStateJSON: string(raw)}); err != nil {
		t.Fatal(err)
	}
	if err := st.OrderDeploymentSleep(dep.ID, 2, proto.SleepModeAbort); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetDeployment(dep.ID)
	if got.DesiredSleep != 2 {
		t.Fatalf("desired sleep=%d, want 2", got.DesiredSleep)
	}
	inst, _ := st.InstanceByUUID("sleep-order-inst")
	var spec proto.InstanceDesiredState
	if err := json.Unmarshal([]byte(inst.DesiredStateJSON), &spec); err != nil {
		t.Fatal(err)
	}
	if spec.SleepMode != proto.SleepModeAbort {
		t.Fatalf("sleep mode=%q, want abort", spec.SleepMode)
	}
}

func TestOrderDeploymentSleepRollsBackWhenAnInstanceSpecIsInvalid(t *testing.T) {
	st := newTestStore(t)
	defer st.Close()
	node, err := st.RegisterNode("sleep-order-rollback-node", HashToken("token"), "sleep-order-rollback-node")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&Deployment{ModelID: 1, NodeID: node.ID, Name: "sleep-rollback", ServedName: "sleep-rollback", Engine: "vllm", Sleepable: true, State: DeployActive})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&Instance{UUID: "sleep-bad-spec", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, DesiredStateJSON: "{"}); err != nil {
		t.Fatal(err)
	}
	if err := st.OrderDeploymentSleep(dep.ID, 1, proto.SleepModeWait); err == nil {
		t.Fatal("invalid instance spec unexpectedly accepted")
	}
	got, _ := st.GetDeployment(dep.ID)
	if got.DesiredSleep != 0 {
		t.Fatalf("deployment desired sleep changed after rollback: %d", got.DesiredSleep)
	}
}

func TestSleepFailureRollsBackOnlyTheMatchingIntent(t *testing.T) {
	st := newTestStore(t)
	defer st.Close()
	node, err := st.RegisterNode("sleep-failure-node", HashToken("token"), "sleep-failure-node")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&Deployment{ModelID: 1, NodeID: node.ID, Name: "sleep-failure", ServedName: "sleep-failure", Engine: "vllm", Sleepable: true, DesiredSleep: 1, State: DeployActive})
	if err != nil {
		t.Fatal(err)
	}
	inst, err := st.CreateInstance(&Instance{UUID: "sleep-failure-inst", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, SleepLevel: 0})
	if err != nil {
		t.Fatal(err)
	}
	target := 1
	failure := proto.InstanceStatus{UUID: inst.UUID, State: proto.InstRunning, SleepLevel: 0, SleepTarget: &target, Phase: proto.PhaseSleep, Message: "sleep failed"}
	if err := st.UpdateInstanceStatus(failure); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetDeployment(dep.ID)
	if got.DesiredSleep != 0 {
		t.Fatalf("matching failed intent was not rolled back: %d", got.DesiredSleep)
	}
	events, err := st.InstanceEvents(inst.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	foundRollback := false
	for _, event := range events {
		if event.Code == "INSTANCE_SLEEP_RESULT" && strings.Contains(event.Data, `"failed_rollback"`) {
			foundRollback = true
		}
	}
	if !foundRollback {
		t.Fatalf("rollback result event missing: %+v", events)
	}

	if err := st.SetDeploymentSleep(dep.ID, 2); err != nil {
		t.Fatal(err)
	}
	failure.SleepTarget = &target
	if err := st.UpdateInstanceStatus(failure); err != nil {
		t.Fatal(err)
	}
	got, _ = st.GetDeployment(dep.ID)
	if got.DesiredSleep != 2 {
		t.Fatalf("newer intent was overwritten by an old failure: %d", got.DesiredSleep)
	}
}

func TestStatusWithoutSleepLevelDoesNotOverwriteObservedLevel(t *testing.T) {
	st := newTestStore(t)
	defer st.Close()
	node, err := st.RegisterNode("sleep-level-preserve-node", HashToken("token"), "sleep-level-preserve-node")
	if err != nil {
		t.Fatal(err)
	}
	inst, err := st.CreateInstance(&Instance{UUID: "sleep-level-preserve", NodeID: node.ID, State: proto.InstRunning, SleepLevel: 2})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: inst.UUID, State: proto.InstStarting, Phase: proto.PhaseHealthCheck, Message: "checking"}); err != nil {
		t.Fatal(err)
	}
	got, err := st.InstanceByUUID(inst.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SleepLevel != 2 {
		t.Fatalf("progress report overwrote sleep level: got %d, want 2", got.SleepLevel)
	}
}

func TestRunningHealthReportClearsObservedSleepLevel(t *testing.T) {
	st := newTestStore(t)
	defer st.Close()
	node, err := st.RegisterNode("sleep-level-clear-node", HashToken("token"), "sleep-level-clear-node")
	if err != nil {
		t.Fatal(err)
	}
	inst, err := st.CreateInstance(&Instance{UUID: "sleep-level-clear", NodeID: node.ID, State: proto.InstRunning, SleepLevel: 1})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: inst.UUID, State: proto.InstRunning, Phase: proto.PhaseHealthCheck}); err != nil {
		t.Fatal(err)
	}
	got, err := st.InstanceByUUID(inst.UUID)
	if err != nil {
		t.Fatal(err)
	}
	if got.SleepLevel != 0 {
		t.Fatalf("running health report did not clear stale sleep level: got %d", got.SleepLevel)
	}
}

// sleepIntentOf reads the sleep level an instance's desired state carries.
func sleepIntentOf(t *testing.T, st *Store, uuid string) (int, string) {
	t.Helper()
	inst, err := st.InstanceByUUID(uuid)
	if err != nil {
		t.Fatal(err)
	}
	var spec proto.InstanceDesiredState
	if err := json.Unmarshal([]byte(inst.DesiredStateJSON), &spec); err != nil {
		t.Fatal(err)
	}
	return spec.DesiredSleep, spec.SleepMode
}

// An order for one replica writes only that instance's intent and leaves the
// deployment-wide level alone; a wake of one replica withdraws a
// deployment-wide order but not the other sleepers' intent. An unknown
// instance fails the whole order.
func TestOrderInstancesSleepWritesOnlyTheNamedInstances(t *testing.T) {
	st := newTestStore(t)
	defer st.Close()
	node, err := st.RegisterNode("replica-sleep-node", HashToken("token"), "replica-sleep-node")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&Deployment{ModelID: 1, NodeID: node.ID, Name: "replica-sleep", ServedName: "replica-sleep", Engine: "vllm", Sleepable: true, Replicas: 2, State: DeployActive})
	if err != nil {
		t.Fatal(err)
	}
	for _, uuid := range []string{"replica-a", "replica-b"} {
		raw, _ := json.Marshal(proto.InstanceDesiredState{UUID: uuid, DesiredState: "running", Sleepable: true})
		if _, err := st.CreateInstance(&Instance{UUID: uuid, DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, DesiredStateJSON: string(raw)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.OrderInstancesSleep(dep.ID, []string{"replica-a"}, 2, proto.SleepModeAbort); err != nil {
		t.Fatal(err)
	}
	if level, mode := sleepIntentOf(t, st, "replica-a"); level != 2 || mode != proto.SleepModeAbort {
		t.Fatalf("replica-a intent = %d/%q, want 2/abort", level, mode)
	}
	if level, _ := sleepIntentOf(t, st, "replica-b"); level != 0 {
		t.Fatalf("replica-b intent = %d, want untouched 0", level)
	}
	if got, _ := st.GetDeployment(dep.ID); got.DesiredSleep != 0 {
		t.Fatalf("an order for one replica set the deployment level to %d", got.DesiredSleep)
	}

	if err := st.OrderDeploymentSleep(dep.ID, 1, proto.SleepModeWait); err != nil {
		t.Fatal(err)
	}
	if err := st.OrderInstancesSleep(dep.ID, []string{"replica-b"}, 0, ""); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetDeployment(dep.ID); got.DesiredSleep != 0 {
		t.Fatalf("waking one replica left the deployment-wide order at %d", got.DesiredSleep)
	}
	if level, _ := sleepIntentOf(t, st, "replica-a"); level != 1 {
		t.Fatalf("replica-a intent = %d after the other replica woke, want 1", level)
	}
	if level, _ := sleepIntentOf(t, st, "replica-b"); level != 0 {
		t.Fatalf("replica-b intent = %d, want 0", level)
	}

	if err := st.OrderInstancesSleep(dep.ID, []string{"replica-a", "missing"}, 0, ""); !errors.Is(err, ErrNotFound) {
		t.Fatalf("order naming an unknown instance = %v, want ErrNotFound", err)
	}
	if level, _ := sleepIntentOf(t, st, "replica-a"); level != 1 {
		t.Fatalf("a refused order changed replica-a to %d", level)
	}
}

// A sleep that fails on one instance rolls back that instance's intent and a
// deployment-wide order at that level; the other instance, which slept, keeps
// its intent rather than being woken.
func TestSleepFailureOnOneReplicaKeepsTheOthersAsleep(t *testing.T) {
	st := newTestStore(t)
	defer st.Close()
	node, err := st.RegisterNode("replica-fail-node", HashToken("token"), "replica-fail-node")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&Deployment{ModelID: 1, NodeID: node.ID, Name: "replica-fail", ServedName: "replica-fail", Engine: "vllm", Sleepable: true, Replicas: 2, State: DeployActive})
	if err != nil {
		t.Fatal(err)
	}
	for _, uuid := range []string{"replica-ok", "replica-bad"} {
		raw, _ := json.Marshal(proto.InstanceDesiredState{UUID: uuid, DesiredState: "running", Sleepable: true})
		if _, err := st.CreateInstance(&Instance{UUID: uuid, DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, DesiredStateJSON: string(raw)}); err != nil {
			t.Fatal(err)
		}
	}
	if err := st.OrderDeploymentSleep(dep.ID, 1, proto.SleepModeWait); err != nil {
		t.Fatal(err)
	}
	target := 1
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: "replica-ok", State: proto.InstRunning, SleepLevel: 1, SleepTarget: &target, Phase: proto.PhaseSleep}); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateInstanceStatus(proto.InstanceStatus{UUID: "replica-bad", State: proto.InstRunning, SleepLevel: 0, SleepTarget: &target, Phase: proto.PhaseSleep, Message: "sleep failed"}); err != nil {
		t.Fatal(err)
	}
	if level, _ := sleepIntentOf(t, st, "replica-bad"); level != 0 {
		t.Fatalf("failed replica intent = %d, want rolled back to 0", level)
	}
	if level, _ := sleepIntentOf(t, st, "replica-ok"); level != 1 {
		t.Fatalf("the replica that slept was woken by the other's failure: intent %d", level)
	}
	if got, _ := st.GetDeployment(dep.ID); got.DesiredSleep != 0 {
		t.Fatalf("deployment-wide order = %d after a replica failed it, want withdrawn", got.DesiredSleep)
	}
}

// An instance whose desired state was built before a deployment-wide sleep
// order committed still starts under that order: the store reads the order in
// the transaction that inserts the row; so does a wake committed after the
// desired state was built while the deployment was still asleep.
func TestCreateInstanceStartsUnderTheDeploymentsStandingSleepOrder(t *testing.T) {
	st := newTestStore(t)
	defer st.Close()
	node, err := st.RegisterNode("stamp-node", HashToken("token"), "stamp-node")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&Deployment{ModelID: 1, NodeID: node.ID, Name: "stamp", ServedName: "stamp", Engine: "vllm", Sleepable: true, Replicas: 2, State: DeployActive})
	if err != nil {
		t.Fatal(err)
	}
	built, _ := json.Marshal(proto.InstanceDesiredState{UUID: "built-awake", DesiredState: "running", Sleepable: true})
	// The order lands after the controller read the deployment and built the
	// desired state, before the row is inserted.
	if err := st.OrderDeploymentSleep(dep.ID, 2, proto.SleepModeWait); err != nil {
		t.Fatal(err)
	}
	inst, err := st.CreateInstance(&Instance{UUID: "built-awake", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstPending, DesiredStateJSON: string(built)})
	if err != nil {
		t.Fatal(err)
	}
	if level, _ := sleepIntentOf(t, st, "built-awake"); level != 2 || inst.SleepIntent() != 2 {
		t.Fatalf("stored intent = %d, returned %d, want 2 from the standing order", level, inst.SleepIntent())
	}

	if err := st.SetDeploymentSleep(dep.ID, 0); err != nil {
		t.Fatal(err)
	}
	// The wake lands after the controller read the deployment while it was
	// still asleep: the instance it built still says 1, and starts awake.
	stale, _ := json.Marshal(proto.InstanceDesiredState{UUID: "built-asleep", DesiredState: "running", Sleepable: true, DesiredSleep: 1})
	if _, err := st.CreateInstance(&Instance{UUID: "built-asleep", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstPending, DesiredStateJSON: string(stale)}); err != nil {
		t.Fatal(err)
	}
	if level, _ := sleepIntentOf(t, st, "built-asleep"); level != 0 {
		t.Fatalf("after the deployment woke the built intent = %d, want 0", level)
	}
}
