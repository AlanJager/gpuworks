package api

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/automation"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The board lists what the schedules do in the next day: a replicas order
// states its count (0 too) and, for a PD role, the role; a sleep order states
// neither.
func TestBoardUpcomingStatesTheReplicaCountAndRole(t *testing.T) {
	rig := newScheduleRig(t)
	soon := time.Now().UTC().Add(time.Hour)
	minute := soon.Hour()*60 + soon.Minute()
	for _, row := range []*store.LifecycleSchedule{
		{DeploymentID: rig.pd.ID, Action: store.ScheduleActionReplicas, Weekdays: automation.AllWeekdays, Minute: minute, Timezone: "UTC", Replicas: 0, Role: "prefill", Enabled: true},
		{DeploymentID: rig.plain.ID, Action: store.ScheduleActionReplicas, Weekdays: automation.AllWeekdays, Minute: minute, Timezone: "UTC", Replicas: 3, Enabled: true},
		{DeploymentID: rig.sleepable.ID, Action: store.ScheduleActionSleep, Weekdays: automation.AllWeekdays, Minute: minute, Timezone: "UTC", SleepLevel: 2, Enabled: true},
	} {
		if err := rig.st.CreateLifecycleSchedule(row); err != nil {
			t.Fatal(err)
		}
	}
	rec := rig.do(rig.session, http.MethodGet, "/api/v1/scheduling/board", "")
	if rec.Code != http.StatusOK {
		t.Fatalf("board = %d %s", rec.Code, rec.Body)
	}
	var board struct {
		Upcoming []json.RawMessage `json:"upcoming"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &board); err != nil || len(board.Upcoming) != 3 {
		t.Fatalf("upcoming = %s (%v)", rec.Body, err)
	}
	byDeployment := map[string]string{}
	for _, raw := range board.Upcoming {
		var item dto.BoardUpcomingAction
		if err := json.Unmarshal(raw, &item); err != nil {
			t.Fatal(err)
		}
		byDeployment[item.Deployment] = string(raw)
	}
	for name, parts := range map[string][]string{
		"pd":     {`"action":"replicas"`, `"replicas":0`, `"role":"prefill"`},
		"plain":  {`"action":"replicas"`, `"replicas":3`},
		"sleepy": {`"action":"sleep"`, `"sleep_level":2`},
	} {
		for _, part := range parts {
			if !strings.Contains(byDeployment[name], part) {
				t.Fatalf("%s upcoming = %s, want %s", name, byDeployment[name], part)
			}
		}
	}
	if strings.Contains(byDeployment["plain"], `"role"`) || strings.Contains(byDeployment["sleepy"], `"role"`) || strings.Contains(byDeployment["sleepy"], `"replicas"`) {
		t.Fatalf("an order that names no role or count states one: plain %s sleepy %s", byDeployment["plain"], byDeployment["sleepy"])
	}
}
