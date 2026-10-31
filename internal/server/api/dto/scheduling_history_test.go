package dto

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

func TestSchedulingHistoryEntryWire(t *testing.T) {
	at := time.Date(2026, 10, 28, 9, 14, 3, 0, time.UTC)
	for _, tc := range []struct {
		name string
		in   SchedulingHistoryEntry
		wire string
	}{
		{
			name: "a manual entry names its actor beside the params",
			in: SchedulingHistoryEntry{ID: 810, CreatedAt: at, DeploymentID: 7, Deployment: "qwen", Kind: "schedule", Code: "REPLICAS_CHANGED", Category: "manual",
				Params: map[string]any{"from": float64(1), "to": float64(3), "actor": "alice"}, Actor: "alice"},
			wire: `{"id":810,"created_at":"2026-10-28T09:14:03Z","deployment_id":7,"deployment":"qwen","kind":"schedule","code":"REPLICAS_CHANGED","category":"manual","params":{"actor":"alice","from":1,"to":3},"actor":"alice"}`,
		},
		{
			name: "a deleted deployment keeps its name and says it is gone; an instance event names the instance",
			in: SchedulingHistoryEntry{ID: 4, CreatedAt: at, DeploymentID: 9, Deployment: "old", DeploymentDeleted: true, Instance: "abc-123", Kind: "schedule", Code: "UNIT_RETRY_EXHAUSTED", Category: "failure",
				Params: map[string]any{"attempts": float64(3)}},
			wire: `{"id":4,"created_at":"2026-10-28T09:14:03Z","deployment_id":9,"deployment":"old","deployment_deleted":true,"instance":"abc-123","kind":"schedule","code":"UNIT_RETRY_EXHAUSTED","category":"failure","params":{"attempts":3}}`,
		},
		{
			name: "an unnamed deployment omits the name, and params are an object even when empty",
			in:   SchedulingHistoryEntry{ID: 2, CreatedAt: at, DeploymentID: 3, Kind: "automation", Code: "AUTO_WAKE", Category: "auto", Params: map[string]any{}},
			wire: `{"id":2,"created_at":"2026-10-28T09:14:03Z","deployment_id":3,"kind":"automation","code":"AUTO_WAKE","category":"auto","params":{}}`,
		},
		{
			name: "the nodes an entry concerns: a known one by UUID and name, a gone one by name only",
			in: SchedulingHistoryEntry{ID: 5, CreatedAt: at, DeploymentID: 7, Kind: "automation", Code: "NODE_REPLACED", Category: "failure",
				Params: map[string]any{"node_uuids": []any{"5f0c-node", "a1b2c3d4-gone"}},
				Nodes:  []SchedulingHistoryNode{{UUID: "5f0c-node", Name: "gpu3"}, {Name: "a1b2c3d4"}}},
			wire: `{"id":5,"created_at":"2026-10-28T09:14:03Z","deployment_id":7,"kind":"automation","code":"NODE_REPLACED","category":"failure","params":{"node_uuids":["5f0c-node","a1b2c3d4-gone"]},"nodes":[{"uuid":"5f0c-node","name":"gpu3"},{"name":"a1b2c3d4"}]}`,
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := marshalString(t, tc.in); got != tc.wire {
				t.Fatalf("marshal\n got %s\nwant %s", got, tc.wire)
			}
			var back SchedulingHistoryEntry
			if err := json.Unmarshal([]byte(tc.wire), &back); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(back, tc.in) {
				t.Fatalf("round trip\n got %+v\nwant %+v", back, tc.in)
			}
		})
	}
}
