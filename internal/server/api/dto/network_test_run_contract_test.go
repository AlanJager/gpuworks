package dto

import (
	"encoding/json"
	"testing"
	"time"
)

// A label run carries its label as label_key and label_value, verbatim; any
// other run carries neither, and no run carries scope_detail. Every run
// carries the nodes under test as node_uuids, [] when there are none.
func TestNetworkTestRunScopeWireContract(t *testing.T) {
	const labelWire = `{"id":4,"status":"done","kind":"tcp","pattern":"mesh","scope":"label","label_key":"pool","label_value":"a=b","bandwidth":false,"node_count":2,"pair_count":2,"passed_count":2,"partial_count":0,"rail_count":0,"rail_passed_count":0,"node_uuids":["a","b"],"pairs":[],"started_at":"2026-09-30T04:00:00Z"}`
	var decoded NetworkTestRun
	if err := json.Unmarshal([]byte(labelWire), &decoded); err != nil {
		t.Fatal(err)
	}
	if decoded.Scope != "label" || decoded.LabelKey != "pool" || decoded.LabelValue != "a=b" || len(decoded.NodeUUIDs) != 2 || decoded.NodeUUIDs[1] != "b" {
		t.Fatalf("decoded = %+v", decoded)
	}
	encoded, err := json.Marshal(decoded)
	if err != nil {
		t.Fatal(err)
	}
	if string(encoded) != labelWire {
		t.Fatalf("round trip = %s\nwant        %s", encoded, labelWire)
	}

	for _, tc := range []struct {
		scope     string
		nodes     []string
		wireNodes string
	}{
		{scope: "all", nodes: []string{"u1", "u2", "u3"}, wireNodes: `["u1","u2","u3"]`},
		{scope: "nodes", nodes: []string{}, wireNodes: `[]`},
	} {
		scope := tc.scope
		encoded, err := json.Marshal(NetworkTestRun{ID: 5, Status: "running", Kind: "tcp", Pattern: "ring", Scope: scope, NodeUUIDs: tc.nodes, Pairs: []NetworkTestPair{}, StartedAt: time.Date(2026, 9, 30, 4, 0, 0, 0, time.UTC)})
		if err != nil {
			t.Fatal(err)
		}
		want := `{"id":5,"status":"running","kind":"tcp","pattern":"ring","scope":"` + scope + `","bandwidth":false,"node_count":0,"pair_count":0,"passed_count":0,"partial_count":0,"rail_count":0,"rail_passed_count":0,"node_uuids":` + tc.wireNodes + `,"pairs":[],"started_at":"2026-09-30T04:00:00Z"}`
		if string(encoded) != want {
			t.Fatalf("%s run = %s\nwant      %s", scope, encoded, want)
		}
	}
}
