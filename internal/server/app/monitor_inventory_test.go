package app

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"sort"
	"sync/atomic"
	"testing"

	"gorm.io/gorm"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/monitor"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

const inventoryGPUMemory = 48 << 30

// inventoryNode registers a node with `gpus` cards of inventoryGPUMemory each.
func inventoryNode(t *testing.T, st *store.Store, name string, gpus int) *store.Node {
	t.Helper()
	node, err := st.RegisterNode(name, store.HashToken("token-"+name), name)
	if err != nil {
		t.Fatal(err)
	}
	for g := 0; g < gpus; g++ {
		gpu := store.GPU{NodeID: node.ID, Index: g, UUID: fmt.Sprintf("%s-gpu%d", name, g), MemoryTotal: inventoryGPUMemory}
		if err := st.DB().Create(&gpu).Error; err != nil {
			t.Fatal(err)
		}
	}
	return node
}

// inventoryInstance adds an instance of the deployment on the node's cards.
// claim > 0 is a memory-limited slice admitted onto a shared card; 0 holds
// the whole card.
func inventoryInstance(t *testing.T, st *store.Store, dep *store.Deployment, node *store.Node, uuid, state, gpus string, claim int64) {
	t.Helper()
	spec, err := json.Marshal(proto.InstanceDesiredState{UUID: uuid, DesiredState: "running", MemoryClaimBytes: claim})
	if err != nil {
		t.Fatal(err)
	}
	in := store.Instance{UUID: uuid, DeploymentID: dep.ID, NodeID: node.ID, State: state, GPUIndexes: gpus, NNodes: 1, DesiredStateJSON: string(spec)}
	if err := st.DB().Create(&in).Error; err != nil {
		t.Fatal(err)
	}
}

func inventoryDeployment(t *testing.T, st *store.Store, name string) *store.Deployment {
	t.Helper()
	dep, err := st.CreateDeployment(&store.Deployment{Name: name, Engine: "vllm", Replicas: 1, State: store.DeployActive})
	if err != nil {
		t.Fatal(err)
	}
	return dep
}

func gpusByCard(inv monitor.Inventory) map[string]int64 {
	out := map[string]int64{}
	for _, g := range inv.GPUs {
		out[fmt.Sprintf("%s/%d", g.UUID, g.Index)] = g.AllocatedBytes
	}
	return out
}

// The inventory charges every card exactly as the per-node ledger does:
// whole cards for an unsliced instance, the admitted slice for a limited
// one, nothing for an instance that holds no claim (stopped, failed) and
// for a node with no instances.
func TestMonitorInventoryChargesCardsLikeThePerNodeLedger(t *testing.T) {
	st := newMonitorTestStore(t)
	dep := inventoryDeployment(t, st, "svc")

	whole := inventoryNode(t, st, "whole", 2)   // one instance on both cards
	sliced := inventoryNode(t, st, "sliced", 2) // two slices on card 1, a stopped one on card 0
	failed := inventoryNode(t, st, "failed", 1) // only a failed instance
	inventoryNode(t, st, "idle", 3)             // no instances at all
	inventoryInstance(t, st, dep, whole, "whole-1", proto.InstRunning, "[0,1]", 0)
	inventoryInstance(t, st, dep, sliced, "slice-a", proto.InstRunning, "[1]", 10<<30)
	inventoryInstance(t, st, dep, sliced, "slice-b", proto.InstStarting, "[1]", 6<<30)
	inventoryInstance(t, st, dep, sliced, "gone", proto.InstStopped, "[0]", 0)
	inventoryInstance(t, st, dep, failed, "broken", proto.InstFailed, "[0]", 0)

	a := &monitorInventoryAdapter{store: st}
	inv, err := a.inventory(context.Background())
	if err != nil {
		t.Fatal(err)
	}

	want := map[string]int64{
		"whole-gpu0/0": inventoryGPUMemory, "whole-gpu1/1": inventoryGPUMemory,
		"sliced-gpu0/0": 0, "sliced-gpu1/1": 16 << 30,
		"failed-gpu0/0": 0,
		"idle-gpu0/0":   0, "idle-gpu1/1": 0, "idle-gpu2/2": 0,
	}
	if got := gpusByCard(inv); !reflect.DeepEqual(got, want) {
		t.Fatalf("allocated bytes by card = %v, want %v", got, want)
	}
	if len(inv.GPUs) != 8 {
		t.Fatalf("GPUs = %d, want one entry per owned card (8)", len(inv.GPUs))
	}
	for _, g := range inv.GPUs {
		if g.MemoryTotalBytes != inventoryGPUMemory {
			t.Errorf("card %s total = %d, want %d", g.UUID, g.MemoryTotalBytes, inventoryGPUMemory)
		}
	}

	// The same figures the per-node ledger gives, node by node.
	nodes, err := st.ListNodes()
	if err != nil {
		t.Fatal(err)
	}
	for i := range nodes {
		n := &nodes[i]
		ledger := planning.GPUAllocations(st, n)
		for _, g := range inv.GPUs {
			if g.NodeUUID == n.UUID && g.AllocatedBytes != ledger[g.Index].AllocatedBytes {
				t.Errorf("node %s card %d: inventory %d, per-node ledger %d", n.Name, g.Index, g.AllocatedBytes, ledger[g.Index].AllocatedBytes)
			}
		}
	}
	names := make([]string, 0, len(inv.Nodes))
	for _, n := range inv.Nodes {
		names = append(names, n.Name)
	}
	sort.Strings(names)
	if !reflect.DeepEqual(names, []string{"failed", "idle", "sliced", "whole"}) {
		t.Fatalf("nodes = %v", names)
	}
}

// The inventory is built in a number of store queries that does not grow with
// the node count: it used to read the instances of each node on their own
// (planning.GPUAllocations), a read per node on every uncached overview.
func TestMonitorInventoryReadsDoNotGrowWithTheNodes(t *testing.T) {
	reads := func(t *testing.T, nodes int) int64 {
		t.Helper()
		st := newMonitorTestStore(t)
		dep := inventoryDeployment(t, st, "svc")
		for i := 0; i < nodes; i++ {
			node := inventoryNode(t, st, fmt.Sprintf("n%d", i), 2)
			inventoryInstance(t, st, dep, node, fmt.Sprintf("run-%d", i), proto.InstRunning, "[0]", 0)
			inventoryInstance(t, st, dep, node, fmt.Sprintf("slice-%d", i), proto.InstRunning, "[1]", 8<<30)
		}
		var count atomic.Int64
		name := fmt.Sprintf("count-inventory-%s-%d", t.Name(), nodes)
		db := st.DB()
		if err := db.Callback().Query().Before("gorm:query").Register(name, func(*gorm.DB) { count.Add(1) }); err != nil {
			t.Fatal(err)
		}
		defer db.Callback().Query().Remove(name)
		a := &monitorInventoryAdapter{store: st}
		inv, err := a.inventory(context.Background())
		if err != nil {
			t.Fatal(err)
		}
		if len(inv.GPUs) != nodes*2 {
			t.Fatalf("GPUs = %d, want %d", len(inv.GPUs), nodes*2)
		}
		return count.Load()
	}
	var few, many int64
	t.Run("3 nodes", func(t *testing.T) { few = reads(t, 3) })
	t.Run("40 nodes", func(t *testing.T) { many = reads(t, 40) })
	t.Logf("inventory store queries: 3 nodes=%d 40 nodes=%d", few, many)
	if few != many {
		t.Fatalf("inventory ran %d queries for 3 nodes and %d for 40; it must not grow with the nodes", few, many)
	}
}
