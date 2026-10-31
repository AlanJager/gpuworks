package dto

import (
	"encoding/json"
	"reflect"
	"testing"
)

// Where a sharing deployment is now, on the wire: a known node carries the
// UUID the UI opens it by; a node no longer known has neither UUID nor name.
func TestTimelineCardsWire(t *testing.T) {
	for _, tc := range []struct {
		name string
		in   TimelineCards
		wire string
	}{
		{"a known node", TimelineCards{NodeID: 3, NodeUUID: "5f0c-node", Node: "gpu3", GPUs: []int{0, 1}}, `{"node_id":3,"node_uuid":"5f0c-node","node":"gpu3","gpus":[0,1]}`},
		{"a node no longer known", TimelineCards{NodeID: 9, GPUs: []int{2}}, `{"node_id":9,"node":"","gpus":[2]}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := marshalString(t, tc.in); got != tc.wire {
				t.Fatalf("marshal\n got %s\nwant %s", got, tc.wire)
			}
			var back TimelineCards
			if err := json.Unmarshal([]byte(tc.wire), &back); err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(back, tc.in) {
				t.Fatalf("round trip\n got %+v\nwant %+v", back, tc.in)
			}
		})
	}
}
