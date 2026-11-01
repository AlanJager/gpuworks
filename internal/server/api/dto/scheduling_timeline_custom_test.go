package dto

import (
	"encoding/json"
	"reflect"
	"testing"
	"time"
)

// A custom range names itself: custom true, the key "custom" (no entry of the
// windows the read lists), its own from and to, and whether the deployments'
// switch counts are known for it.
func TestTimelineCustomRangeWire(t *testing.T) {
	from := time.Date(2026, 10, 29, 8, 30, 0, 0, time.UTC)
	in := SchedulingTimeline{
		Scope:   TimelineScopeNode,
		Window:  TimelineWindow{From: from, To: from.Add(90 * time.Minute), Hours: 1, Key: TimelineWindowCustom, Seconds: 5400, Custom: true},
		Windows: []TimelineWindowOption{{Key: "15m", Seconds: 900}},
		Nodes:   []TimelineNode{}, Deployments: []TimelineDeployment{}, SwitchesAvailable: false,
	}
	want := `{"scope":"node","window":{"from":"2026-10-29T08:30:00Z","to":"2026-10-29T10:00:00Z","hours":1,"key":"custom","seconds":5400,"custom":true},` +
		`"windows":[{"key":"15m","seconds":900}],"nodes":[],"deployments":[],"switches_available":false}`
	if got := marshalString(t, in); got != want {
		t.Fatalf("marshal\n got %s\nwant %s", got, want)
	}
	var back SchedulingTimeline
	if err := json.Unmarshal([]byte(want), &back); err != nil || !reflect.DeepEqual(back, in) {
		t.Fatalf("round trip %+v %v", back, err)
	}
	if TimelineWindowCustom != "custom" {
		t.Fatalf("custom key = %q", TimelineWindowCustom)
	}
}
