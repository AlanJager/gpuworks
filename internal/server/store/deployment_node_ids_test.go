package store

import (
	"reflect"
	"testing"
)

// The node_ids column is read in several places that each want a different
// answer for an empty or unreadable value; these tables pin each reader's
// answer for every stored shape.
var storedNodeIDShapes = []string{"", "null", "[]", "[3,5]", "not json"}

// NodeIDList falls back to the primary node whenever the column names no
// node, whether it is empty, an empty array or unreadable.
func TestDeploymentNodeIDListFallsBackToThePrimaryNode(t *testing.T) {
	want := map[string][]uint{
		"":         {9},
		"null":     {9},
		"[]":       {9},
		"[3,5]":    {3, 5},
		"not json": {9},
	}
	for _, raw := range storedNodeIDShapes {
		t.Run(raw, func(t *testing.T) {
			d := &Deployment{NodeID: 9, NodeIDs: raw}
			if got := d.NodeIDList(); !reflect.DeepEqual(got, want[raw]) {
				t.Fatalf("NodeIDList(%q) = %v, want %v", raw, got, want[raw])
			}
		})
	}
}

// The eviction guard fails closed on an unreadable list: an active deployment
// whose participants cannot be read must keep every node's cache.
func TestDeploymentReferencesNodeFailsClosedOnUnreadableNodeIDs(t *testing.T) {
	for _, tc := range []struct {
		raw    string
		nodeID uint
		want   bool
	}{
		{raw: "", nodeID: 9, want: true}, // the primary node always counts
		{raw: "", nodeID: 3},
		{raw: "null", nodeID: 3},
		{raw: "[]", nodeID: 3},
		{raw: "[3,5]", nodeID: 5, want: true},
		{raw: "[3,5]", nodeID: 4},
		{raw: "not json", nodeID: 3, want: true},
	} {
		dep := Deployment{NodeID: 9, NodeIDs: tc.raw}
		if got := deploymentReferencesNode(dep, tc.nodeID); got != tc.want {
			t.Errorf("deploymentReferencesNode(node_ids=%q, node %d) = %v, want %v", tc.raw, tc.nodeID, got, tc.want)
		}
	}
}

// StoredNodeIDs reports the column as written and leaves the fallback to the
// caller: empty and null are "no list", an empty array is an empty list, and
// an unreadable value is an error rather than a silent empty list.
func TestDeploymentStoredNodeIDs(t *testing.T) {
	for _, tc := range []struct {
		raw     string
		want    []uint
		wantErr bool
	}{
		{raw: ""},
		{raw: "null"},
		{raw: "[]", want: []uint{}},
		{raw: "[3,5]", want: []uint{3, 5}},
		{raw: "not json", wantErr: true},
	} {
		t.Run(tc.raw, func(t *testing.T) {
			got, err := (&Deployment{NodeID: 9, NodeIDs: tc.raw}).StoredNodeIDs()
			if (err != nil) != tc.wantErr {
				t.Fatalf("StoredNodeIDs(%q) err = %v, want error %v", tc.raw, err, tc.wantErr)
			}
			if !reflect.DeepEqual(got, tc.want) {
				t.Fatalf("StoredNodeIDs(%q) = %#v, want %#v", tc.raw, got, tc.want)
			}
		})
	}
}
