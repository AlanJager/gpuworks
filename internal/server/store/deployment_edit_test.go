package store

import (
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/nodelabel"
)

func editFixture(t *testing.T, state DeploymentState, insts ...Instance) (*Store, *Deployment) {
	t.Helper()
	st := newTestStore(t)
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("edit-node", HashToken("edit-token"), "edit")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeploymentWithRequirements(&Deployment{
		Name: "edit", ModelID: 1, NodeID: node.ID, Engine: "vllm", State: state, Replicas: 2,
		QuickParams: `{"max_model_len":8192}`, RoutingPolicy: "round_robin", PlacementStrategy: PlacementSpread,
	}, nodelabel.Requirements{"gpuworks.ai/vendor": {"nvidia"}})
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range insts {
		row.DeploymentID, row.NodeID = dep.ID, node.ID
		if _, err := st.CreateInstance(&row); err != nil {
			t.Fatal(err)
		}
	}
	read, err := st.GetDeployment(dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	return st, read
}

func readReqs(t *testing.T, st *Store, id uint) nodelabel.Requirements {
	t.Helper()
	reqs, err := st.DeploymentRequirements(id)
	if err != nil {
		t.Fatal(err)
	}
	return reqs
}

// A rolling edit writes the new configuration and asks for a rolling restart
// in the same write: failed attempts are retired, running replicas wait for
// the roll.
func TestEditDeploymentRollingWritesConfigAndRequestsRoll(t *testing.T) {
	st, read := editFixture(t, DeployActive, Instance{UUID: "e-ok", State: proto.InstRunning}, Instance{UUID: "e-failed", State: proto.InstFailed})
	next := *read
	next.QuickParams = `{"max_model_len":16384}`
	next.RoutingPolicy = "least_loaded"
	reqs := nodelabel.Requirements{"gpuworks.ai/vendor": {"nvidia"}, "zone": {"a", "b"}}
	if err := st.EditDeployment(read, readReqs(t, st, read.ID), DeploymentEdit{Next: &next, Requirements: reqs, Apply: EditApplyRolling}); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetDeployment(read.ID)
	if got.QuickParams != next.QuickParams || got.RoutingPolicy != "least_loaded" || got.RestartRequestedAt == nil {
		t.Fatalf("after edit: quick=%s routing=%s restart=%v", got.QuickParams, got.RoutingPolicy, got.RestartRequestedAt)
	}
	if stored := readReqs(t, st, read.ID); !sameRequirements(stored, reqs) {
		t.Fatalf("requirements = %v, want %v", stored, reqs)
	}
	if inst, _ := st.InstanceByUUID("e-failed"); inst.State != proto.InstRemoving {
		t.Fatalf("failed attempt = %s, want removing", inst.State)
	}
	if inst, _ := st.InstanceByUUID("e-ok"); inst.State != proto.InstRunning {
		t.Fatalf("running replica = %s, want left for the roll", inst.State)
	}
}

func TestEditDeploymentApplyAllAndNone(t *testing.T) {
	st, read := editFixture(t, DeployActive, Instance{UUID: "a-ok", State: proto.InstRunning})
	next := *read
	next.ExtraArgs = "--enforce-eager"
	if err := st.EditDeployment(read, readReqs(t, st, read.ID), DeploymentEdit{Next: &next, Apply: EditApplyNone}); err != nil {
		t.Fatal(err)
	}
	if inst, _ := st.InstanceByUUID("a-ok"); inst.State != proto.InstRunning {
		t.Fatalf("apply none touched an instance: %s", inst.State)
	}
	read, _ = st.GetDeployment(read.ID)
	next = *read
	next.ExtraArgs = ""
	if err := st.EditDeployment(read, readReqs(t, st, read.ID), DeploymentEdit{Next: &next, Apply: EditApplyAll}); err != nil {
		t.Fatal(err)
	}
	if inst, _ := st.InstanceByUUID("a-ok"); inst.State != proto.InstRemoving {
		t.Fatalf("apply all left an instance %s", inst.State)
	}
	if got, _ := st.GetDeployment(read.ID); got.ExtraArgs != "" || got.RestartRequestedAt != nil {
		t.Fatalf("after apply all: extra=%q restart=%v", got.ExtraArgs, got.RestartRequestedAt)
	}
}

// The edit was judged against what was read. A configuration, requirement or
// desired-state change since then refuses it, and nothing is written.
func TestEditDeploymentRefusesWhenTheDeploymentMovedOn(t *testing.T) {
	st, read := editFixture(t, DeployActive)
	reqs := readReqs(t, st, read.ID)
	if err := st.SetDeploymentRoutingPolicy(read.ID, "least_loaded"); err != nil {
		t.Fatal(err)
	}
	next := *read
	next.QuickParams = `{"max_model_len":4096}`
	if err := st.EditDeployment(read, reqs, DeploymentEdit{Next: &next}); !errors.Is(err, ErrStateChanged) {
		t.Fatalf("edit over a changed routing policy: err = %v", err)
	}
	if got, _ := st.GetDeployment(read.ID); got.QuickParams != read.QuickParams {
		t.Fatalf("refused edit wrote %s", got.QuickParams)
	}

	read, _ = st.GetDeployment(read.ID)
	if err := st.SetDeploymentRequirements(read.ID, nodelabel.Requirements{"zone": {"c"}}, PlacementSpread); err != nil {
		t.Fatal(err)
	}
	next = *read
	if err := st.EditDeployment(read, reqs, DeploymentEdit{Next: &next}); !errors.Is(err, ErrStateChanged) {
		t.Fatalf("edit over changed requirements: err = %v", err)
	}

	// What the controller keeps moving is not a conflict.
	read, _ = st.GetDeployment(read.ID)
	reqs = readReqs(t, st, read.ID)
	if err := st.RequestRollingRestart(read.ID, read.CreatedAt); err != nil {
		t.Fatal(err)
	}
	next = *read
	next.Replicas = 3
	if err := st.EditDeployment(read, reqs, DeploymentEdit{Next: &next}); err != nil {
		t.Fatalf("a rolling restart in progress refused the edit: %v", err)
	}

	if err := st.EditDeployment(&Deployment{ID: 9999}, nil, DeploymentEdit{Next: &Deployment{ID: 9999}}); !errors.Is(err, ErrNotFound) {
		t.Fatalf("unknown deployment: err = %v", err)
	}
}

// A rolling or immediate apply needs an active deployment; a stopped one is
// refused whole, configuration included, unless the edit starts it.
func TestEditDeploymentStoppedSavesOrStarts(t *testing.T) {
	st, read := editFixture(t, DeployStopped)
	next := *read
	next.QuickParams = `{"max_model_len":4096}`
	if err := st.EditDeployment(read, readReqs(t, st, read.ID), DeploymentEdit{Next: &next, Apply: EditApplyRolling}); !errors.Is(err, ErrStateChanged) {
		t.Fatalf("rolling a stopped deployment: err = %v", err)
	}
	if got, _ := st.GetDeployment(read.ID); got.QuickParams != read.QuickParams {
		t.Fatalf("the refused apply kept its configuration write: %s", got.QuickParams)
	}
	if err := st.EditDeployment(read, readReqs(t, st, read.ID), DeploymentEdit{Next: &next, Apply: EditApplyNone}); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetDeployment(read.ID); got.State != DeployStopped || got.QuickParams != next.QuickParams {
		t.Fatalf("save on a stopped deployment: state=%s quick=%s", got.State, got.QuickParams)
	}
	read, _ = st.GetDeployment(read.ID)
	next = *read
	next.StartTimeoutSec = 900
	if err := st.EditDeployment(read, readReqs(t, st, read.ID), DeploymentEdit{Next: &next, Start: true}); err != nil {
		t.Fatal(err)
	}
	if got, _ := st.GetDeployment(read.ID); got.State != DeployActive || got.StartTimeoutSec != 900 {
		t.Fatalf("save and start: state=%s timeout=%d", got.State, got.StartTimeoutSec)
	}
}

// Waking with the edit withdraws the deployment-wide sleep order and every
// instance's, so the instances the restart brings up start awake.
func TestEditDeploymentWakeWithdrawsSleepOrder(t *testing.T) {
	st, read := editFixture(t, DeployActive, Instance{UUID: "w-asleep", State: proto.InstRunning, DesiredStateJSON: `{"desired_sleep":1}`})
	if err := st.SetDeploymentSleep(read.ID, 1); err != nil {
		t.Fatal(err)
	}
	read, _ = st.GetDeployment(read.ID)
	next := *read
	next.QuickParams = `{"max_model_len":4096}`
	if err := st.EditDeployment(read, readReqs(t, st, read.ID), DeploymentEdit{Next: &next, Apply: EditApplyAll, Wake: true}); err != nil {
		t.Fatal(err)
	}
	got, _ := st.GetDeployment(read.ID)
	if got.DesiredSleep != 0 {
		t.Fatalf("desired_sleep = %d, want 0", got.DesiredSleep)
	}
	inst, _ := st.InstanceByUUID("w-asleep")
	if inst.State != proto.InstRemoving || inst.SleepIntent() != 0 {
		t.Fatalf("asleep instance: state=%s intent=%d", inst.State, inst.SleepIntent())
	}
}

// A failure inside the write rolls the whole edit back: the configuration,
// the requirements and the instances stay as they were.
func TestEditDeploymentRollsBackOnFailure(t *testing.T) {
	st, read := editFixture(t, DeployActive, Instance{UUID: "r-ok", State: proto.InstRunning})
	reqs := readReqs(t, st, read.ID)
	next := *read
	next.QuickParams = `{"max_model_len":1024}`
	err := st.EditDeployment(read, reqs, DeploymentEdit{Next: &next, Requirements: nodelabel.Requirements{"zone": {"z"}}, Apply: "sideways"})
	if err == nil {
		t.Fatal("unknown apply accepted")
	}
	if got, _ := st.GetDeployment(read.ID); got.QuickParams != read.QuickParams {
		t.Fatalf("configuration written despite the failure: %s", got.QuickParams)
	}
	if stored := readReqs(t, st, read.ID); !sameRequirements(stored, reqs) {
		t.Fatalf("requirements written despite the failure: %v", stored)
	}
	if inst, _ := st.InstanceByUUID("r-ok"); inst.State != proto.InstRunning {
		t.Fatalf("instance moved despite the failure: %s", inst.State)
	}
	if err := st.EditDeployment(read, reqs, DeploymentEdit{Next: &Deployment{ID: read.ID + 1}}); err == nil {
		t.Fatal("an edit of another row accepted")
	}
}
