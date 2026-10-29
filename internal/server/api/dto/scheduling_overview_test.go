package dto

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// A card is an array on the wire, the occupants left out when there are none.
func TestOverviewCardWire(t *testing.T) {
	for _, tc := range []struct {
		name string
		card OverviewCard
		wire string
	}{
		{"free", OverviewCard{Index: 0, Model: 0, MemoryMiB: 49152}, `[0,0,49152,0,0,0]`},
		{"awake and asleep sharing", OverviewCard{Index: 3, Model: 2, MemoryMiB: 81920, AwakePct: 45, SleepingPct: 5, Flags: OverviewSwitching, Occupants: []int{0, 3}}, `[3,2,81920,45,5,2,[0,3]]`},
		{"held outside", OverviewCard{Index: 7, Model: 1, MemoryMiB: 24576, Flags: OverviewHeldOutside | OverviewUnhealthy}, `[7,1,24576,0,0,5]`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			raw, err := json.Marshal(tc.card)
			if err != nil || string(raw) != tc.wire {
				t.Fatalf("marshal = %s %v, want %s", raw, err, tc.wire)
			}
			var back OverviewCard
			if err := json.Unmarshal([]byte(tc.wire), &back); err != nil || !reflect.DeepEqual(back, tc.card) {
				t.Fatalf("unmarshal = %+v %v, want %+v", back, err, tc.card)
			}
		})
	}
	for _, bad := range []string{`[1,2,3]`, `[1,2,3,4,5,6,7,8]`, `{"index":1}`, `[1,2,3,4,5,"x"]`, `[1,2,3,4,5,6,"x"]`} {
		var card OverviewCard
		if err := json.Unmarshal([]byte(bad), &card); err == nil {
			t.Errorf("%s was read as a card", bad)
		}
	}
}

// The overview's keys and defaults on the wire: empty lists are [] and a node
// without labels or contention carries neither key.
func TestSchedulingOverviewWire(t *testing.T) {
	overview := SchedulingOverview{
		Summary: OverviewSummary{
			Nodes: 2, OnlineNodes: 1, Cards: 3, Awake: 1, Asleep: 0, Free: 1, HeldOutside: 0, Unhealthy: 0, Offline: 1,
			ByModel:    []OverviewModelSummary{{Model: "L20", Cards: 3, Awake: 1, Free: 1, Offline: 1}},
			Contention: OverviewContention{WaitingWakes: 1, HeldRequests: 4, YieldsLastHour: 2},
		},
		Models:      []string{"L20"},
		Deployments: []OverviewDeployment{{ID: 7, Name: "qwen", Preset: "shared"}},
		Nodes: []OverviewNode{
			{UUID: "n1", Name: "gpu-1", Online: true, Labels: map[string]string{"zone": "a"}, Contention: 6, Cards: []OverviewCard{{MemoryMiB: 49152, AwakePct: 100, Occupants: []int{0}}, {Index: 1, MemoryMiB: 49152}}},
			{UUID: "n2", Name: "gpu-2", Cards: []OverviewCard{{MemoryMiB: 49152}}},
		},
	}
	raw, err := json.Marshal(overview)
	if err != nil {
		t.Fatal(err)
	}
	want := `{"summary":{"nodes":2,"online_nodes":1,"cards":3,"awake":1,"asleep":0,"free":1,"held_outside":0,"unhealthy":0,"offline":1,` +
		`"by_model":[{"model":"L20","cards":3,"awake":1,"asleep":0,"free":1,"held_outside":0,"unhealthy":0,"offline":1}],` +
		`"contention":{"waiting_wakes":1,"held_requests":4,"yields_last_hour":2}},` +
		`"models":["L20"],"deployments":[{"id":7,"name":"qwen","preset":"shared"}],` +
		`"nodes":[{"uuid":"n1","name":"gpu-1","online":true,"labels":{"zone":"a"},"contention":6,"cards":[[0,0,49152,100,0,0,[0]],[1,0,49152,0,0,0]]},` +
		`{"uuid":"n2","name":"gpu-2","online":false,"cards":[[0,0,49152,0,0,0]]}]}`
	if string(raw) != want {
		t.Fatalf("wire =\n%s\nwant\n%s", raw, want)
	}
	var back SchedulingOverview
	if err := json.Unmarshal(raw, &back); err != nil || !reflect.DeepEqual(back, overview) {
		t.Fatalf("round trip = %+v %v", back, err)
	}
	if strings.Contains(string(raw), `"occupants"`) {
		t.Fatal("a card must not carry a key for its occupants")
	}
}
