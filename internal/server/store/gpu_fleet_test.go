package store

import (
	"sort"
	"strings"
	"testing"
)

func TestOnlineGPUGroupsGroupsCardsPerNodeAndModelOnlineOnly(t *testing.T) {
	s := newTestStore(t)
	mk := func(uuid, state string, gpus ...GPU) *Node {
		n, err := s.RegisterNode(uuid, HashToken(uuid), uuid)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.db.Model(&Node{}).Where("id = ?", n.ID).Update("state", state).Error; err != nil {
			t.Fatal(err)
		}
		for i := range gpus {
			gpus[i].NodeID, gpus[i].Index = n.ID, i
			if err := s.db.Create(&gpus[i]).Error; err != nil {
				t.Fatal(err)
			}
		}
		return n
	}
	h100 := GPU{Vendor: "nvidia", Name: "H100", ComputeCapability: "9.0", MemoryTotal: 80 << 30}
	small := GPU{Vendor: "nvidia", Name: "H100", ComputeCapability: "9.0", MemoryTotal: 79 << 30}
	l4 := GPU{Vendor: "nvidia", Name: "L4", ComputeCapability: "8.9", MemoryTotal: 24 << 30}
	// The health levels, as a probe reports them: a warning still serves,
	// an error does not, and a card with no level stated has had nothing
	// found about it (the level a report without one is stored at is
	// decided once, in newCardRow).
	warn := GPU{Vendor: "nvidia", Name: "H100", ComputeCapability: "9.0", MemoryTotal: 80 << 30, HealthStatus: "Warning"}
	broken := GPU{Vendor: "nvidia", Name: "H100", ComputeCapability: "9.0", MemoryTotal: 80 << 30, HealthStatus: "Error"}
	flagged := GPU{Vendor: "nvidia", Name: "L4", ComputeCapability: "8.9", MemoryTotal: 24 << 30, HealthStatus: "Error"}
	a := mk("fleet-a", NodeStateOnline, h100, small, l4, warn, broken, flagged)
	b := mk("fleet-b", NodeStateOnline, h100)
	mk("fleet-off", "offline", h100, h100, h100, h100)

	groups, err := s.OnlineGPUGroups()
	if err != nil {
		t.Fatal(err)
	}
	sort.Slice(groups, func(i, j int) bool {
		if groups[i].NodeID != groups[j].NodeID {
			return groups[i].NodeID < groups[j].NodeID
		}
		return groups[i].Name < groups[j].Name
	})
	want := []OnlineGPUGroup{
		{NodeID: a.ID, Vendor: "nvidia", Name: "H100", ComputeCapability: "9.0", Cards: 3, Unusable: 1, MaxMemory: 80 << 30},
		{NodeID: a.ID, Vendor: "nvidia", Name: "L4", ComputeCapability: "8.9", Cards: 1, Unusable: 1, MaxMemory: 24 << 30},
		{NodeID: b.ID, Vendor: "nvidia", Name: "H100", ComputeCapability: "9.0", Cards: 1, MaxMemory: 80 << 30},
	}
	if len(groups) != len(want) {
		t.Fatalf("groups = %+v", groups)
	}
	for i := range want {
		if groups[i] != want[i] {
			t.Fatalf("group %d = %+v, want %+v", i, groups[i], want[i])
		}
	}
}

// The join walks the online nodes by the state index and each one's cards by
// idx_node_card; only those rows reach the grouping, not every GPU row.
func TestOnlineGPUGroupsQueryPlan(t *testing.T) {
	s := newTestStore(t)
	var plan []struct {
		Detail string `gorm:"column:detail"`
	}
	if err := s.db.Raw(`EXPLAIN QUERY PLAN SELECT n.id, COUNT(*) FROM gpus g JOIN nodes n ON n.id = g.node_id WHERE n.state = 'online' GROUP BY n.id, n.framework, g.vendor, g.name, g.arch_id, g.compute_capability`).Scan(&plan).Error; err != nil {
		t.Fatal(err)
	}
	var lines []string
	for _, p := range plan {
		lines = append(lines, p.Detail)
	}
	joined := strings.Join(lines, " | ")
	if !strings.Contains(joined, "idx_nodes_state") || !strings.Contains(joined, "idx_node_card") {
		t.Fatalf("plan uses no index: %s", joined)
	}
	// The cards must be looked up per node, never scanned: a fleet's rows
	// outnumber any one node's by three orders of magnitude.
	if strings.Contains(joined, "SCAN g") {
		t.Fatalf("the cards are scanned rather than looked up per node: %s", joined)
	}
	t.Log(joined)
}

func TestNodeCardLayoutsReadsTopologyAndCardNames(t *testing.T) {
	s := newTestStore(t)
	n, err := s.RegisterNode("layout-a", HashToken("layout-a"), "layout-a")
	if err != nil {
		t.Fatal(err)
	}
	topo := `{"GPUs":[{"Index":0},{"Index":1},{"Index":2}],"FastCliques":[[0,1]]}`
	if err := s.db.Model(&Node{}).Where("id = ?", n.ID).Update("topo_json", topo).Error; err != nil {
		t.Fatal(err)
	}
	bare, err := s.RegisterNode("layout-b", HashToken("layout-b"), "layout-b")
	if err != nil {
		t.Fatal(err)
	}
	for i, name := range []string{"H100", "H100", "L4"} {
		if err := s.db.Create(&GPU{NodeID: n.ID, Index: i, Name: name}).Error; err != nil {
			t.Fatal(err)
		}
	}
	if err := s.db.Create(&GPU{NodeID: bare.ID, Index: 0, Name: "L4"}).Error; err != nil {
		t.Fatal(err)
	}
	layouts, err := s.NodeCardLayouts([]uint{n.ID, bare.ID})
	if err != nil {
		t.Fatal(err)
	}
	a := layouts[n.ID]
	if a.Topology == nil || len(a.Topology.FastCliques) != 1 || a.Names[0] != "H100" || a.Names[2] != "L4" {
		t.Fatalf("layout a = %+v", a)
	}
	if b := layouts[bare.ID]; b.Topology != nil || b.Names[0] != "L4" {
		t.Fatalf("layout b = %+v", b)
	}
	if empty, err := s.NodeCardLayouts(nil); err != nil || len(empty) != 0 {
		t.Fatalf("empty = %v %v", empty, err)
	}
}
