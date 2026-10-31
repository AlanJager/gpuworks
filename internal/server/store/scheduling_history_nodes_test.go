package store

import (
	"encoding/json"
	"reflect"
	"strings"
	"testing"
)

// nodesFixture is two nodes (one named, one known by hostname only) and
// instances on them, one not placed.
func nodesFixture(t *testing.T, s *Store) (a, b Node) {
	t.Helper()
	a = Node{UUID: "node-a", Name: "gpu-a", Hostname: "host-a"}
	b = Node{UUID: "node-b", Hostname: "host-b"}
	for _, n := range []*Node{&a, &b} {
		if err := s.DB().Create(n).Error; err != nil {
			t.Fatal(err)
		}
	}
	for _, inst := range []Instance{
		{UUID: "inst-a1", DeploymentID: 1, NodeID: a.ID},
		{UUID: "inst-a2", DeploymentID: 1, NodeID: a.ID},
		{UUID: "inst-b1", DeploymentID: 1, NodeID: b.ID},
		{UUID: "inst-unplaced", DeploymentID: 1},
	} {
		if err := s.DB().Create(&inst).Error; err != nil {
			t.Fatal(err)
		}
	}
	return a, b
}

func lastEventParams(t *testing.T, s *Store) map[string]any {
	t.Helper()
	var ev InstanceEvent
	if err := s.DB().Order("id desc").First(&ev).Error; err != nil {
		t.Fatal(err)
	}
	out := map[string]any{}
	if err := json.Unmarshal([]byte(ev.Data), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

// A scheduling event that names instances keeps the nodes they are on, read
// while the instances exist; the writer's own node, a code outside the
// history and an event naming no placed instance are left alone.
func TestSchedulingEventsRecordTheirNodes(t *testing.T) {
	s := newTestStore(t)
	nodesFixture(t, s)

	params := map[string]any{"trigger": "preempt", "instances": []string{"inst-b1", "inst-a1", "inst-a2"}}
	s.AddDeploymentEventCode(1, EventKindAutomation, "AUTO_SLEEP", params)
	if got := lastEventParams(t, s)[EventParamNodeUUIDs]; !reflect.DeepEqual(got, []any{"node-a", "node-b"}) {
		t.Fatalf("AUTO_SLEEP node_uuids = %#v, want the instances' nodes, sorted, each once", got)
	}
	if _, changed := params[EventParamNodeUUIDs]; changed {
		t.Fatal("the writer's params were changed")
	}

	// The row's own instance counts (UNIT_RETRY_EXHAUSTED is written on it).
	inst, err := s.InstanceByUUID("inst-b1")
	if err != nil {
		t.Fatal(err)
	}
	s.AddInstanceEventCode(inst, EventKindSchedule, EventUnitRetryExhausted, map[string]any{"attempts": 3})
	if got := lastEventParams(t, s)[EventParamNodeUUIDs]; !reflect.DeepEqual(got, []any{"node-b"}) {
		t.Fatalf("UNIT_RETRY_EXHAUSTED node_uuids = %#v", got)
	}

	// A single "instance" param counts too (REPLICA_RESTARTED).
	s.AddDeploymentEventCode(1, EventKindSchedule, EventReplicaRestarted, map[string]any{"instance": "inst-a2"})
	if got := lastEventParams(t, s)[EventParamNodeUUIDs]; !reflect.DeepEqual(got, []any{"node-a"}) {
		t.Fatalf("REPLICA_RESTARTED node_uuids = %#v", got)
	}

	for _, c := range []struct {
		name   string
		code   string
		params map[string]any
		want   any
	}{
		{"the writer named the nodes", "NODE_REPLACED", map[string]any{"instances": []string{"inst-a1"}, EventParamNodeUUIDs: []string{"node-x"}}, []any{"node-x"}},
		{"the writer named its node", EventDeploymentUnschedulable, map[string]any{"node": "node-b", "instances": []string{"inst-a1"}}, nil},
		{"not a scheduling code", EventDeploymentEdited, map[string]any{"instances": []string{"inst-a1"}}, nil},
		{"no placed instance", EventWakeOrdered, map[string]any{"instances": []string{"inst-unplaced", "inst-gone"}}, nil},
		{"no instance at all", EventReplicasChanged, map[string]any{"from": 1, "to": 2}, nil},
	} {
		s.AddDeploymentEventCode(1, EventKindSchedule, c.code, c.params)
		if got := lastEventParams(t, s)[EventParamNodeUUIDs]; !reflect.DeepEqual(got, c.want) {
			t.Errorf("%s: node_uuids = %#v, want %#v", c.name, got, c.want)
		}
	}
}

// One read names a page's nodes by UUID and by the live instances it names,
// each through a unique index: the cost is the page's, never the fleet's.
func TestEventNodeRefsReadsThePageInOneIndexedQuery(t *testing.T) {
	s := newTestStore(t)
	a, b := nodesFixture(t, s)

	byUUID, byInstance, err := s.EventNodeRefs([]string{"node-b", "node-gone"}, []string{"inst-a1", "inst-unplaced", "inst-gone"})
	if err != nil {
		t.Fatal(err)
	}
	if want := map[string]NodeRef{"node-b": {UUID: "node-b", Name: "host-b"}}; !reflect.DeepEqual(byUUID, want) {
		t.Fatalf("by UUID = %+v, want %+v", byUUID, want)
	}
	if want := map[string]NodeRef{"inst-a1": {UUID: "node-a", Name: "gpu-a"}}; !reflect.DeepEqual(byInstance, want) {
		t.Fatalf("by instance = %+v, want %+v", byInstance, want)
	}
	// Either list may be empty.
	if byUUID, byInstance, err = s.EventNodeRefs(nil, []string{"inst-b1"}); err != nil || len(byUUID) != 0 || byInstance["inst-b1"].UUID != "node-b" {
		t.Fatalf("instances only = %+v %+v %v", byUUID, byInstance, err)
	}
	if byUUID, byInstance, err = s.EventNodeRefs([]string{"node-a"}, nil); err != nil || byUUID["node-a"].Name != "gpu-a" || len(byInstance) != 0 {
		t.Fatalf("nodes only = %+v %+v %v", byUUID, byInstance, err)
	}

	refs, err := s.NodeRefsByID([]uint{a.ID, b.ID, 999})
	if err != nil {
		t.Fatal(err)
	}
	if want := map[uint]NodeRef{a.ID: {UUID: "node-a", Name: "gpu-a"}, b.ID: {UUID: "node-b", Name: "host-b"}}; !reflect.DeepEqual(refs, want) {
		t.Fatalf("refs by id = %+v, want %+v", refs, want)
	}

	var plan []struct{ Detail string }
	if err := s.DB().Raw("EXPLAIN QUERY PLAN "+eventNodeRefsQuery, []string{"node-a"}, []string{"inst-a1"}).Scan(&plan).Error; err != nil {
		t.Fatal(err)
	}
	for _, step := range plan {
		if strings.HasPrefix(step.Detail, "SCAN") {
			t.Fatalf("the read scans a table: %+v", plan)
		}
	}
}
