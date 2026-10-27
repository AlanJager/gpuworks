package dto

import (
	"encoding/json"
	"reflect"
	"testing"
)

func marshalString(t *testing.T, v any) string {
	t.Helper()
	raw, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func TestNodeReplacementWire(t *testing.T) {
	if got, want := marshalString(t, NodeReplacement{AfterSec: 600}), `{"after_sec":600}`; got != want {
		t.Fatalf("node replacement = %s, want %s", got, want)
	}
	// Never is after_sec 0, and it is still written: no omitempty.
	if got, want := marshalString(t, NodeReplacement{}), `{"after_sec":0}`; got != want {
		t.Fatalf("node replacement never = %s, want %s", got, want)
	}
	var repl NodeReplacement
	if err := json.Unmarshal([]byte(`{"after_sec":120}`), &repl); err != nil || repl != (NodeReplacement{AfterSec: 120}) {
		t.Fatalf("node replacement decoded = %+v %v", repl, err)
	}
	if err := json.Unmarshal([]byte(`{"after_sec":"120"}`), &repl); err == nil {
		t.Fatal("a string after_sec was accepted")
	}
}

func TestWakeRefusalHeldOutsideWire(t *testing.T) {
	plain := WakeRefusal{Instances: []string{"i1"}, Node: "n1", GPUIndexes: []int{0}, ShortfallGiB: "2.5", Occupants: []string{"dep"}}
	if got, want := marshalString(t, plain), `{"instances":["i1"],"node":"n1","gpu_indexes":[0],"shortfall_gib":"2.5","occupants":["dep"]}`; got != want {
		t.Fatalf("held_outside false must be omitted:\n got %s\nwant %s", got, want)
	}
	held := plain
	held.HeldOutside = true
	const wire = `{"instances":["i1"],"node":"n1","gpu_indexes":[0],"shortfall_gib":"2.5","occupants":["dep"],"held_outside":true}`
	if got := marshalString(t, held); got != wire {
		t.Fatalf("held_outside true:\n got %s\nwant %s", got, wire)
	}
	var back WakeRefusal
	if err := json.Unmarshal([]byte(wire), &back); err != nil || !reflect.DeepEqual(back, held) {
		t.Fatalf("round trip = %+v %v", back, err)
	}
}

// The deployment carries its node replacement on every response, the never
// (0) setting included.
func TestDeploymentNodeReplacementWire(t *testing.T) {
	keys := func(d Deployment) map[string]json.RawMessage {
		var m map[string]json.RawMessage
		if err := json.Unmarshal([]byte(marshalString(t, d)), &m); err != nil {
			t.Fatal(err)
		}
		return m
	}
	if m := keys(Deployment{NodeReplacement: NodeReplacement{AfterSec: 600}}); string(m["node_replacement"]) != `{"after_sec":600}` {
		t.Fatalf("node_replacement = %s", m["node_replacement"])
	}
	if m := keys(Deployment{}); string(m["node_replacement"]) != `{"after_sec":0}` {
		t.Fatalf("a zero setting must still be written: %s", m["node_replacement"])
	}
	var back Deployment
	if err := json.Unmarshal([]byte(`{"node_replacement":{"after_sec":7200}}`), &back); err != nil || back.NodeReplacement.AfterSec != 7200 {
		t.Fatalf("decoded = %+v %v", back.NodeReplacement, err)
	}
}
