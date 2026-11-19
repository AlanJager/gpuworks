package app

import (
	"context"
	"errors"
	"log/slog"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/slurmsweep"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestSlurmSweepRemovesOnlyNodesTheirClusterNoLongerLists(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	c, err := st.CreateSlurmCluster(store.SlurmCluster{Name: "hpc"})
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	enroll := func(name, state string, seen time.Time) store.Node {
		n, _, err := st.EnrollSlurmNode(c.Token, "demo", name)
		if err != nil {
			t.Fatal(err)
		}
		if err := st.DB().Model(&store.Node{}).Where("id = ?", n.ID).Updates(map[string]any{"state": state, "last_seen_at": seen}).Error; err != nil {
			t.Fatal(err)
		}
		return *n
	}
	enroll("peer", store.NodeStateOnline, now)
	gone := enroll("gone", store.NodeStateOffline, now.Add(-time.Hour))
	listed := enroll("rebooting", store.NodeStateOffline, now.Add(-time.Hour))
	recent := enroll("blip", store.NodeStateOffline, now)
	busy := enroll("busy", store.NodeStateOffline, now.Add(-time.Hour))
	// A GPU node whose Slurm GRES has no gpu (or that sits in a hidden
	// partition) is in the cluster all the same: its entry lists it by name.
	nogres := enroll("nogres", store.NodeStateOffline, now.Add(-time.Hour))
	if _, err := st.ReplaceSlurmGPUNodes(c.ID, []store.SlurmGPUNode{{Name: "peer", State: "idle", GPUs: 1}, {Name: "rebooting", State: "down", GPUs: 1}, {Name: "nogres", ListedOnly: true}}); err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&store.Instance{NodeID: busy.ID, UUID: "inst-1", State: proto.InstRunning}).Error; err != nil {
		t.Fatal(err)
	}
	var kicked []string
	sweeper := slurmsweep.New(slog.New(slog.DiscardHandler), slurmSweepStore{st}, nil, func(u string) { kicked = append(kicked, u) })
	sweep := func() { sweeper.Sweep(context.Background(), now.Add(-15*time.Minute)) }
	exists := func(n store.Node) bool {
		_, err := st.NodeByUUID(n.UUID)
		if err != nil && !errors.Is(err, store.ErrNotFound) {
			t.Fatal(err)
		}
		return err == nil
	}
	// The first sweep only marks: a list that was one report short must not
	// remove a node the next report brings back.
	sweep()
	if len(kicked) != 0 || !exists(gone) {
		t.Fatalf("first sweep removed %v", kicked)
	}
	sweep()
	if exists(gone) {
		t.Fatal("the node that left remains")
	}
	for _, n := range []store.Node{listed, recent, busy, nogres} {
		if !exists(n) {
			t.Fatalf("node %s was removed", n.SlurmNode)
		}
	}
	if len(kicked) != 1 || kicked[0] != gone.UUID {
		t.Fatalf("kicked %v", kicked)
	}
	// A node that came back in the list between two sweeps loses its mark: it
	// takes two sweeps in a row that find it gone again.
	late := enroll("late", store.NodeStateOffline, now.Add(-time.Hour))
	sweep()
	if _, err := st.ReplaceSlurmGPUNodes(c.ID, []store.SlurmGPUNode{{Name: "peer", State: "idle", GPUs: 1}, {Name: "rebooting", State: "down", GPUs: 1}, {Name: "late", State: "idle", GPUs: 1}, {Name: "nogres", ListedOnly: true}}); err != nil {
		t.Fatal(err)
	}
	sweep()
	if _, err := st.ReplaceSlurmGPUNodes(c.ID, []store.SlurmGPUNode{{Name: "peer", State: "idle", GPUs: 1}, {Name: "rebooting", State: "down", GPUs: 1}, {Name: "nogres", ListedOnly: true}}); err != nil {
		t.Fatal(err)
	}
	sweep()
	if !exists(late) {
		t.Fatal("a node listed in between was removed on its first sweep after it left again")
	}
	sweep()
	if exists(late) || len(kicked) != 2 || kicked[1] != late.UUID {
		t.Fatalf("late exists %v, kicked %v", exists(late), kicked)
	}
}

// cloudNodes is a cloud that lists the given nodes for every entry.
type cloudNodes []string

func (c cloudNodes) NodeNames(context.Context, uint) ([]string, error) { return c, nil }

// The last node with an agent of an E-HPC entry was released (2026-11-18): no
// agent is left to report a list without it, so the list still names it and
// the console said "GPU nodes 1, 1 joined" for good. The cloud no longer has
// it, so the sweep removes it, and with it its name on the list. An entry of
// its own (no cloud) is not asked, and keeps its node.
func TestSweepRemovesTheLastNodeOfACloudEntryTheCloudNoLongerHas(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	now := time.Now()
	enrollIn := func(name string) (store.SlurmCluster, store.Node) {
		c, err := st.CreateSlurmCluster(store.SlurmCluster{Name: name})
		if err != nil {
			t.Fatal(err)
		}
		n, _, err := st.EnrollSlurmNode(c.Token, name, "compute001")
		if err != nil {
			t.Fatal(err)
		}
		if err := st.DB().Model(&store.Node{}).Where("id = ?", n.ID).Updates(map[string]any{"state": store.NodeStateOffline, "last_seen_at": now.Add(-time.Hour)}).Error; err != nil {
			t.Fatal(err)
		}
		if _, err := st.ReplaceSlurmGPUNodes(c.ID, []store.SlurmGPUNode{{Name: "compute001", State: "idle", GPUs: 1}}); err != nil {
			t.Fatal(err)
		}
		return *c, *n
	}
	cloudEntry, released := enrollIn("ehpc")
	providerID := uint(9)
	if err := st.DB().Model(&store.SlurmCluster{}).Where("id = ?", cloudEntry.ID).Updates(map[string]any{"provider_id": providerID, "provider_cluster_id": "ehpc-hz-1"}).Error; err != nil {
		t.Fatal(err)
	}
	_, own := enrollIn("onprem")
	sweeper := slurmsweep.New(slog.New(slog.DiscardHandler), slurmSweepStore{st}, cloudNodes{"compute002"}, func(string) {})
	for i := 0; i < 2; i++ {
		sweeper.Sweep(context.Background(), now.Add(-15*time.Minute))
	}
	if _, err := st.NodeByUUID(released.UUID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("the released node remains: %v", err)
	}
	if _, err := st.NodeByUUID(own.UUID); err != nil {
		t.Fatalf("the entry of its own lost its node: %v", err)
	}
	counts, err := st.SlurmGPUNodeCounts()
	if err != nil {
		t.Fatal(err)
	}
	if counts[cloudEntry.ID] != 0 {
		t.Fatalf("the cloud entry still lists %d GPU nodes", counts[cloudEntry.ID])
	}
}
