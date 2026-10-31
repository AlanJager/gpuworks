package dto

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// The scheduling activity on the wire: every key spelled, counts in the
// reasons' order, a node no longer known with an empty name; it reads back
// as it was.
func TestSchedulingActivityWire(t *testing.T) {
	from := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	in := SchedulingActivity{
		From: from, To: from.Add(30 * time.Minute), BucketSeconds: 900,
		Reasons:  []ActivityReason{{Key: "idle_yield", Count: 3}, {Key: "queued", Count: 0}},
		Buckets:  []ActivityBucket{{Start: from, Counts: []int64{1, 0}}, {Start: from.Add(15 * time.Minute), Counts: []int64{2, 0}}},
		TopCards: []ActivityCard{{NodeUUID: "5f0c-node", Node: "gpu3", GPUIndex: 2, Switches: 7}, {NodeUUID: "gone", GPUIndex: 0, Switches: 1}},
	}
	want := `{"from":"2026-10-30T12:00:00Z","to":"2026-10-30T12:30:00Z","bucket_seconds":900,` +
		`"reasons":[{"key":"idle_yield","count":3},{"key":"queued","count":0}],` +
		`"buckets":[{"start":"2026-10-30T12:00:00Z","counts":[1,0]},{"start":"2026-10-30T12:15:00Z","counts":[2,0]}],` +
		`"top_cards":[{"node_uuid":"5f0c-node","node":"gpu3","gpu_index":2,"switches":7},{"node_uuid":"gone","node":"","gpu_index":0,"switches":1}],` +
		`"beyond_retention":false}`
	if got := marshalString(t, in); got != want {
		t.Fatalf("marshal\n got %s\nwant %s", got, want)
	}
	var back SchedulingActivity
	if err := json.Unmarshal([]byte(want), &back); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(back, in) {
		t.Fatalf("round trip\n got %+v\nwant %+v", back, in)
	}
}

// A timeline's window names itself and the windows the read offers.
func TestTimelineWindowWire(t *testing.T) {
	from := time.Date(2026, 10, 30, 12, 0, 0, 0, time.UTC)
	in := SchedulingTimeline{
		Scope:   TimelineScopeFleet,
		Window:  TimelineWindow{From: from, To: from.Add(15 * time.Minute), Hours: 0, Key: "15m", Seconds: 900},
		Windows: []TimelineWindowOption{{Key: "15m", Seconds: 900}, {Key: "6h", Seconds: 21600}},
		Nodes:   []TimelineNode{}, Deployments: []TimelineDeployment{},
	}
	want := `{"scope":"fleet","window":{"from":"2026-10-30T12:00:00Z","to":"2026-10-30T12:15:00Z","hours":0,"key":"15m","seconds":900},` +
		`"windows":[{"key":"15m","seconds":900},{"key":"6h","seconds":21600}],"nodes":[],"deployments":[]}`
	if got := marshalString(t, in); got != want {
		t.Fatalf("marshal\n got %s\nwant %s", got, want)
	}
	var back SchedulingTimeline
	if err := json.Unmarshal([]byte(want), &back); err != nil || !reflect.DeepEqual(back, in) {
		t.Fatalf("round trip %+v %v", back, err)
	}
}
