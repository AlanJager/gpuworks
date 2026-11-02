package dto

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// What a timeline's deployment figures count, on the wire: the first key,
// "node" for a node page's (only the instances on its nodes), "fleet" for the
// fleet's sharing stats.
func TestSchedulingTimelineScopeWire(t *testing.T) {
	from := time.Date(2026, 10, 31, 6, 0, 0, 0, time.UTC)
	window := TimelineWindow{From: from, To: from.Add(6 * time.Hour), Hours: 6, Key: "6h", Seconds: 21600}
	const windowWire = `"window":{"from":"2026-10-31T06:00:00Z","to":"2026-10-31T12:00:00Z","hours":6,"key":"6h","seconds":21600,"custom":false},"windows":[{"key":"6h","seconds":21600}],"nodes":[],"deployments":[],"switches_available":true}`
	for _, tc := range []struct {
		scope, wire string
	}{
		{TimelineScopeNode, `{"scope":"node",` + windowWire},
		{TimelineScopeFleet, `{"scope":"fleet",` + windowWire},
	} {
		t.Run(tc.scope, func(t *testing.T) {
			in := SchedulingTimeline{
				Scope: tc.scope, Window: window, Windows: []TimelineWindowOption{{Key: "6h", Seconds: 21600}},
				Nodes: []TimelineNode{}, Deployments: []TimelineDeployment{}, SwitchesAvailable: true,
			}
			if got := marshalString(t, in); got != tc.wire {
				t.Fatalf("marshal\n got %s\nwant %s", got, tc.wire)
			}
			var back SchedulingTimeline
			if err := json.Unmarshal([]byte(tc.wire), &back); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(back, in) {
				t.Fatalf("round trip\n got %+v\nwant %+v", back, in)
			}
		})
	}
	if TimelineScopeNode != "node" || TimelineScopeFleet != "fleet" {
		t.Fatalf("scopes = %q, %q", TimelineScopeNode, TimelineScopeFleet)
	}
}
