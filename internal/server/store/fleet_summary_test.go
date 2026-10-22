package store

import (
	"sort"
	"testing"
)

func TestFleetSummaryCountsNodesVersionsAndOwnedCards(t *testing.T) {
	s := newTestStore(t)
	empty, err := s.FleetSummary()
	if err != nil || empty.Nodes != 0 || len(empty.AgentVersions) != 0 || len(empty.GPUModels) != 0 {
		t.Fatalf("an empty fleet = %+v, %v", empty, err)
	}
	mk := func(uuid, state, agent string, gpus ...GPU) {
		n, err := s.RegisterNode(uuid, HashToken(uuid), uuid)
		if err != nil {
			t.Fatal(err)
		}
		if err := s.db.Model(&Node{}).Where("id = ?", n.ID).Updates(map[string]any{"state": state, "agent_version": agent}).Error; err != nil {
			t.Fatal(err)
		}
		for i := range gpus {
			gpus[i].NodeID, gpus[i].Index = n.ID, i
			if err := s.db.Create(&gpus[i]).Error; err != nil {
				t.Fatal(err)
			}
		}
	}
	a100, h20 := GPU{Name: "A100"}, GPU{Name: "H20"}
	lent := GPU{Name: "A100", Ownership: "lent_to_vm"}
	mk("sum-1", NodeStateOnline, "v0.9.1", a100, a100, lent)
	mk("sum-2", NodeStateOffline, "v0.9.1", h20)
	mk("sum-3", NodeStateOnline, "v0.9.0", a100)
	mk("sum-4", NodeStateConnecting, "")

	got, err := s.FleetSummary()
	if err != nil {
		t.Fatal(err)
	}
	if got.Nodes != 4 || got.OnlineNodes != 2 {
		t.Fatalf("nodes = %d online %d, want 4 and 2", got.Nodes, got.OnlineNodes)
	}
	order := func(in []NameCount) []NameCount {
		sort.Slice(in, func(i, j int) bool { return in[i].Name < in[j].Name })
		return in
	}
	versions := order(got.AgentVersions)
	if len(versions) != 3 || versions[0] != (NameCount{"", 1}) || versions[1] != (NameCount{"v0.9.0", 1}) || versions[2] != (NameCount{"v0.9.1", 2}) {
		t.Fatalf("agent versions = %+v", versions)
	}
	models := order(got.GPUModels)
	if len(models) != 2 || models[0] != (NameCount{"A100", 3}) || models[1] != (NameCount{"H20", 1}) {
		t.Fatalf("gpu models = %+v, want a card another owner holds left out", models)
	}
}
