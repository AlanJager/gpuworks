package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

func (r *autoRig) deploymentNodes(t *testing.T, id uint) [][]string {
	t.Helper()
	rec := r.do(r.session, http.MethodGet, fmt.Sprintf("/api/v1/deployments/%d", id), "")
	var out dto.Deployment
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil || rec.Code != http.StatusOK {
		t.Fatalf("get = %d %s %v", rec.Code, rec.Body, err)
	}
	return replicaNodes(out)
}

// The scheduling automation sets a replica count through the same path as
// PUT /deployments/{id}/replicas: an automatically placed multi-node
// deployment gets its new replica's nodes picked as the API picks them, and
// every refusal reaches the caller as a product error with its code.
func TestReplicaScalerPlacesAnAutomaticMultiNodeDeployment(t *testing.T) {
	r := newAutoRig(t, 4)
	scale := ReplicaScaler(Deps{Store: r.st})
	dep := r.mustCreate(t, r.multiBody("scaled", ""))

	if err := scale(dep.ID, 2); err != nil {
		t.Fatal(err)
	}
	// The same nodes as TestScaleAutoMultiNodeGrowsByCount's request gets.
	nodes := r.deploymentNodes(t, dep.ID)
	if stored := r.storedDeployment(t, dep.ID); stored.Replicas != 2 || !stored.UnitsAuto || len(nodes) != 2 ||
		!slices.Equal(nodes[0], []string{"au-1", "au-2"}) || !slices.Equal(nodes[1], []string{"au-2", "au-1"}) {
		t.Fatalf("after growing to 2: replicas %d, nodes %v", stored.Replicas, nodes)
	}
	// The same count again changes nothing, and a count over the limit is the limit.
	if err := scale(dep.ID, 2); err != nil || len(r.deploymentNodes(t, dep.ID)) != 2 {
		t.Fatalf("same count: %v", err)
	}
	if err := scale(dep.ID, 1); err != nil {
		t.Fatal(err)
	}
	if stored := r.storedDeployment(t, dep.ID); stored.Replicas != 1 || len(r.deploymentNodes(t, dep.ID)) != 1 {
		t.Fatalf("after shrinking to 1: replicas %d", stored.Replicas)
	}

	// No room for another replica: the platform's own placement refusal.
	r.occupy(t, "au-1", 1)
	r.occupy(t, "au-2", 1)
	r.occupy(t, "au-3", 0, 1)
	r.occupy(t, "au-4", 0, 1)
	err := scale(dep.ID, 2)
	if err == nil || productErrorCode(t, err) != ErrorUnitPlacementNone {
		t.Fatalf("no room: err = %v, want %s", err, ErrorUnitPlacementNone)
	}
	if stored := r.storedDeployment(t, dep.ID); stored.Replicas != 1 {
		t.Fatalf("replicas = %d after a refused grow", stored.Replicas)
	}

	// Switched to placement by hand since the schedule was written: the order
	// is refused with the cause, nothing changes.
	if rec := r.do(r.session, http.MethodPut, schedPath(r.storedDeployment(t, dep.ID), "placement-mode"), `{"mode":"manual"}`); rec.Code != http.StatusOK {
		t.Fatalf("placement mode = %d %s", rec.Code, rec.Body)
	}
	if err := scale(dep.ID, 2); err == nil || productErrorCode(t, err) != "SCHEDULE_REPLICAS_MANUAL_PLACEMENT" {
		t.Fatalf("manual placement: err = %v", err)
	}
	if stored := r.storedDeployment(t, dep.ID); stored.Replicas != 1 {
		t.Fatalf("replicas = %d after a refused order", stored.Replicas)
	}
}

func TestReplicaScalerSingleNodeAndPD(t *testing.T) {
	rig := newScheduleRig(t)
	scale := ReplicaScaler(Deps{Store: rig.st})
	if err := scale(rig.plain.ID, 3); err != nil {
		t.Fatal(err)
	}
	if got, _ := rig.st.GetDeployment(rig.plain.ID); got.Replicas != 3 {
		t.Fatalf("replicas = %d, want 3", got.Replicas)
	}
	if err := scale(rig.plain.ID, 99); err != nil {
		t.Fatal(err)
	}
	if got, _ := rig.st.GetDeployment(rig.plain.ID); got.Replicas != 32 {
		t.Fatalf("replicas = %d, want the limit 32", got.Replicas)
	}
	if err := scale(rig.pd.ID, 2); err == nil || productErrorCode(t, err) != "SCHEDULE_REPLICAS_SHAPE" {
		t.Fatalf("PD: err = %v", err)
	}
	if err := scale(rig.multi.ID, 2); err == nil || productErrorCode(t, err) != "SCHEDULE_REPLICAS_MANUAL_PLACEMENT" {
		t.Fatalf("multi-node placed by hand: err = %v", err)
	}
	if err := scale(99999, 2); err == nil {
		t.Fatal("an unknown deployment is an error")
	}
}
