package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A deployment refused a placement waits on the board only while a replica
// is still unplaced: once an instance fills it (the agents came back after a
// server restart, say) the refusal is history and it leaves the list.
func TestBoardPendingListsOnlyDeploymentsStillWaiting(t *testing.T) {
	rig := newScheduleRig(t)
	rig.st.AddDeploymentEventCode(rig.plain.ID, store.EventKindSchedule, store.EventDeploymentUnschedulable,
		map[string]any{"scheduler": map[string]any{"code": "pool_empty", "params": map[string]any{"offline": 1}}})
	pending := func() []string {
		t.Helper()
		rec := rig.do(rig.session, http.MethodGet, "/api/v1/scheduling/board", "")
		if rec.Code != http.StatusOK {
			t.Fatalf("board = %d %s", rec.Code, rec.Body)
		}
		var board struct {
			Pending []struct {
				Deployment string `json:"deployment"`
			} `json:"pending"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &board); err != nil {
			t.Fatal(err)
		}
		var names []string
		for _, p := range board.Pending {
			names = append(names, p.Deployment)
		}
		return names
	}
	if got := pending(); len(got) != 1 || got[0] != rig.plain.Name {
		t.Fatalf("pending before placement = %v, want [%s]", got, rig.plain.Name)
	}
	spec, _ := json.Marshal(proto.InstanceDesiredState{UUID: "plain-1", DesiredState: "running"})
	if _, err := rig.st.CreateInstance(&store.Instance{UUID: "plain-1", DeploymentID: rig.plain.ID, State: proto.InstRunning, GPUIndexes: "[0]", NNodes: 1, DesiredStateJSON: string(spec)}); err != nil {
		t.Fatal(err)
	}
	if got := pending(); len(got) != 0 {
		t.Fatalf("pending after an instance filled the replica = %v, want none", got)
	}
}
