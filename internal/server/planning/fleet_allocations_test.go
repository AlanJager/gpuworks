package planning

import (
	"encoding/json"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// FleetAllocations charges the rows the caller holds, of any state, with the
// figures the per-node ledger gives: a replica awake on two cards, a sleeper
// beside it with its residue, and a stopped instance that charges nothing.
func TestFleetAllocationsMatchThePerNodeLedger(t *testing.T) {
	c := newYieldCards(t, 2)
	a := c.deployment(t, "a", "anytime")
	b := c.deployment(t, "b", "timeslice")
	c.instance(t, a, "a-1", "[0,1]", 0)
	c.instance(t, b, "b-1", "[1]", 1)
	c.instance(t, b, "b-gone", "[0]", 0)
	if err := c.st.DB().Model(&store.Instance{}).Where("uuid = ?", "b-gone").Update("state", proto.InstStopped).Error; err != nil {
		t.Fatal(err)
	}
	// The replica was admitted with 10 GiB of each card, which leaves room
	// for the sleeper's residue beside it.
	claim, _ := json.Marshal(proto.InstanceDesiredState{UUID: "a-1", DesiredState: "running", Sleepable: true, MemoryClaimBytes: 10 << 30})
	if err := c.st.DB().Model(&store.Instance{}).Where("uuid = ?", "a-1").Select("DesiredStateJSON").Updates(&store.Instance{DesiredStateJSON: string(claim)}).Error; err != nil {
		t.Fatal(err)
	}
	idle, err := c.st.RegisterNode("idle-node", store.HashToken("idle"), "idle-node")
	if err != nil {
		t.Fatal(err)
	}
	idle.GPUs = append(idle.GPUs, store.GPU{Index: 0, UUID: "idle-0", Vendor: "nvidia", MemoryTotal: 16 << 30})

	instances, err := c.st.ListInstances()
	if err != nil {
		t.Fatal(err)
	}
	deployments, err := c.st.ListDeployments()
	if err != nil {
		t.Fatal(err)
	}
	nodes := []store.Node{*c.node, *idle}
	got, err := FleetAllocations(c.st, nodes, instances, deployments)
	if err != nil {
		t.Fatal(err)
	}
	for i := range nodes {
		want := GPUAllocations(c.st, &nodes[i])
		fleet := got[nodes[i].ID]
		if len(fleet) != len(want) {
			t.Fatalf("node %s: %d cards charged, per-node ledger %d", nodes[i].Name, len(fleet), len(want))
		}
		for index, w := range want {
			g := fleet[index]
			if g.AllocatedBytes != w.AllocatedBytes || g.SleepingBytes != w.SleepingBytes || len(g.Instances) != len(w.Instances) {
				t.Errorf("node %s card %d: fleet %d/%d/%d instances, per-node %d/%d/%d", nodes[i].Name, index,
					g.AllocatedBytes, g.SleepingBytes, len(g.Instances), w.AllocatedBytes, w.SleepingBytes, len(w.Instances))
			}
		}
	}
	if card1 := got[c.node.ID][1]; card1.AllocatedBytes == 0 || card1.SleepingBytes == 0 || len(card1.Instances) != 2 {
		t.Fatalf("card 1 = %+v, want the replica awake and the sleeper's residue, nothing for the stopped row", card1)
	}
	if len(got[idle.ID]) != 0 {
		t.Fatalf("a node with no instances charges %v, want nothing", got[idle.ID])
	}
}
