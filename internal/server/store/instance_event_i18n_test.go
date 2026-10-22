package store

import (
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
)

func TestInstanceEventCodeMigrationAndHistoricalCompatibility(t *testing.T) {
	st, err := Open("file:instance_event_i18n?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	version, err := st.CurrentMigration()
	if err != nil {
		t.Fatal(err)
	}
	if version < 56 || !st.DB().Migrator().HasColumn(&InstanceEvent{}, "Code") {
		t.Fatalf("instance event code migration missing: version=%d", version)
	}

	st.AddInstanceEventCode(&Instance{ID: 1, UUID: "uuid-1", DeploymentID: 7}, "state", instanceEventStateChanged, map[string]any{"state": "running"})
	st.AddInstanceEvent(1, "state", "historical free text", "legacy-data")
	events, err := st.InstanceEvents(1, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 2 {
		t.Fatalf("events = %+v", events)
	}
	if events[0].Code != instanceEventStateChanged || events[0].Message != "" {
		t.Fatalf("structured event = %+v", events[0])
	}
	var params map[string]any
	if err := json.Unmarshal([]byte(events[0].Data), &params); err != nil || params["state"] != "running" {
		t.Fatalf("structured event params = %+v, err=%v", params, err)
	}
	if events[1].Code != "" || events[1].Message != "historical free text" || events[1].Data != "legacy-data" {
		t.Fatalf("historical event = %+v", events[1])
	}
}
