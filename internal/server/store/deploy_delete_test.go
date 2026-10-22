package store

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

// Deleting a deployment is a statement about the live configuration, not an
// instruction to erase what happened. The instance rows go; the events stay and
// remain addressable by UUID, because that history is what a failure is
// diagnosed from after the fact.
func TestDeleteDeploymentRemovesInstancesButKeepsEvents(t *testing.T) {
	st, err := Open("file:store_delete_deployment_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&Deployment{Name: "delete-me", NodeID: 1, State: "deleting"})
	if err != nil {
		t.Fatal(err)
	}
	inst, err := st.CreateInstance(&Instance{UUID: "delete-me-inst", DeploymentID: dep.ID, NodeID: 1, State: "starting"})
	if err != nil {
		t.Fatal(err)
	}
	st.AddInstanceEvent(inst.ID, "state", "created", "")
	st.AddInstanceEventCode(inst, "state", instanceEventStateChanged,
		map[string]any{"state": "failed", "detail": "engine exited"})

	if err := st.DeleteDeployment(dep.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := st.GetDeployment(dep.ID); err != ErrNotFound {
		t.Fatalf("GetDeployment error = %v, want ErrNotFound", err)
	}
	instances, err := st.InstancesForDeployment(dep.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(instances) != 0 {
		t.Fatalf("instances after delete = %+v", instances)
	}

	// The events survive, and what they say about the failure is still readable.
	byUUID, err := st.InstanceEventsByUUID(inst.UUID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(byUUID) != 2 {
		t.Fatalf("events kept by uuid = %d, want 2: %+v", len(byUUID), byUUID)
	}
	if byUUID[1].Code != instanceEventStateChanged {
		t.Fatalf("failure event lost its code: %+v", byUUID[1])
	}
	if byUUID[1].Data != `{"detail":"engine exited","state":"failed"}` {
		t.Fatalf("failure event lost its params: %q", byUUID[1].Data)
	}

	// They are also reachable through the deployment, which is what an operator
	// has left after the deployment row itself is gone.
	byDep, err := st.DeploymentEvents(dep.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(byDep) != 2 {
		t.Fatalf("events kept by deployment = %d, want 2", len(byDep))
	}
	for _, ev := range byDep {
		if ev.InstanceUUID != inst.UUID || ev.DeploymentID != dep.ID {
			t.Fatalf("event lost its self-identity: %+v", ev)
		}
	}

	if err := st.DeleteDeployment(dep.ID); err != ErrNotFound {
		t.Fatalf("second delete error = %v, want ErrNotFound", err)
	}
}

// A scheduling outcome is decided before any instance exists, so it cannot hang
// off one. It is recorded against the deployment instead, and that is the only
// trace a deployment that could never be placed leaves anywhere the API reaches.
func TestDeploymentEventWithoutInstance(t *testing.T) {
	st, err := Open("file:store_deployment_event_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&Deployment{Name: "unschedulable", NodeID: 1, State: "active"})
	if err != nil {
		t.Fatal(err)
	}
	st.AddDeploymentEventCode(dep.ID, EventKindSchedule, EventDeploymentUnschedulable,
		map[string]any{"code": "no_free_gpu", "tp": 2})

	ev, found, err := st.LatestScheduleEvent(dep.ID)
	if err != nil || !found {
		t.Fatalf("LatestScheduleEvent found=%v err=%v", found, err)
	}
	if ev.InstanceID != 0 || ev.InstanceUUID != "" {
		t.Fatalf("scheduling event should carry no instance identity: %+v", ev)
	}
	if ev.DeploymentID != dep.ID || ev.Kind != EventKindSchedule {
		t.Fatalf("scheduling event mis-scoped: %+v", ev)
	}
	if ev.Code != EventDeploymentUnschedulable {
		t.Fatalf("scheduling event code = %q", ev.Code)
	}

	// A deployment that was never blocked reports no scheduling entry at all,
	// which is what lets the caller tell "queued" apart from "rejected".
	if _, found, err := st.LatestScheduleEvent(dep.ID + 999); err != nil || found {
		t.Fatalf("unblocked deployment reported found=%v err=%v", found, err)
	}

	// Oldest-first ordering matters for the timeline the API renders.
	if err := st.DeleteDeployment(dep.ID); err != nil {
		t.Fatal(err)
	}
	kept, err := st.DeploymentEvents(dep.ID, 0)
	if err != nil || len(kept) != 1 {
		t.Fatalf("scheduling event did not survive deployment delete: %d err=%v", len(kept), err)
	}
}
