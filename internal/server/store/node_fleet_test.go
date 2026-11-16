package store

import (
	"context"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
)

// fleetEvents collects what NodeFleetTopic carries, read the way a module
// reads it.
func fleetEvents(t *testing.T, s *Store) func() []NodeFleetChange {
	t.Helper()
	sub := s.Bus.Subscribe(256, NodeFleetTopic)
	t.Cleanup(sub.Close)
	return func() []NodeFleetChange {
		t.Helper()
		var out []NodeFleetChange
		for {
			select {
			case ev := <-sub.C():
				c, ok := NodeFleetChangeOf(ev)
				if !ok {
					t.Fatalf("unreadable fleet event %+v", ev)
				}
				out = append(out, c)
			case <-time.After(50 * time.Millisecond):
				return out
			}
		}
	}
}

func wantFleet(t *testing.T, got []NodeFleetChange, want ...NodeFleetChange) {
	t.Helper()
	if len(got) != len(want) {
		t.Fatalf("fleet events = %+v, want %+v", got, want)
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("fleet events = %+v, want %+v", got, want)
		}
	}
}

// Every node insert path, a status report that changes the card count or
// vendor, and a delete are announced on NodeFleetTopic by node id; a
// routine report that changes neither, and any other node change, are not.
func TestNodeFleetTopicAnnouncesInsertsFactChangesAndDeletes(t *testing.T) {
	s := openKubeStore(t)
	next := fleetEvents(t, s)

	registered := newHoldNode(t, s, "fleet-a")
	regID := registered.row().ID
	pending, _, err := s.CreateNodePending("fleet-b")
	if err != nil {
		t.Fatal(err)
	}
	cluster, err := s.CreateKubeCluster(KubeCluster{Name: "fleet"})
	if err != nil {
		t.Fatal(err)
	}
	kube, _, err := s.EnrollKubeNode(cluster.Token, "uid", "fleet-k")
	if err != nil {
		t.Fatal(err)
	}
	wantFleet(t, next(), NodeFleetChange{regID, NodeInserted}, NodeFleetChange{pending.ID, NodeInserted}, NodeFleetChange{kube.ID, NodeInserted})

	card := func(i int, bdf string) proto.GPUInfo {
		return proto.GPUInfo{Index: i, UUID: "GPU-fleet-" + bdf, Vendor: "nvidia", PCIeBDF: bdf, MemoryTotalBytes: 24 << 30, Healthy: true}
	}
	sys := proto.SysInfo{RuntimeDriver: "docker"}
	if got := registered.report(sys, card(0, "0000:3f:00.0")).GPUCount; got != 1 {
		t.Fatalf("cards = %d", got)
	}
	wantFleet(t, next(), NodeFleetChange{regID, NodeFactsChanged})
	registered.report(sys, card(0, "0000:3f:00.0"))
	if _, err := s.RenameNode(registered.uuid, "renamed"); err != nil {
		t.Fatal(err)
	}
	wantFleet(t, next()) // the same facts again, and a rename, change nothing a holder decides on
	registered.report(sys, card(0, "0000:3f:00.0"), card(1, "0000:40:00.0"))
	wantFleet(t, next(), NodeFleetChange{regID, NodeFactsChanged})

	if err := s.DeleteNode(pending.UUID); err != nil {
		t.Fatal(err)
	}
	wantFleet(t, next(), NodeFleetChange{pending.ID, NodeDeleted})

	// The newest node deleted, the next insert still takes a new id: an
	// event's id names one node for good.
	if err := s.DeleteNode(kube.UUID); err != nil {
		t.Fatal(err)
	}
	again, _, err := s.CreateNodePending("fleet-c")
	if err != nil {
		t.Fatal(err)
	}
	if again.ID <= kube.ID {
		t.Fatalf("a new node took id %d after node %d was deleted; ids must not be reused", again.ID, kube.ID)
	}
	wantFleet(t, next(), NodeFleetChange{kube.ID, NodeDeleted}, NodeFleetChange{again.ID, NodeInserted})
}

func TestNodeFleetChangeOfReadsOnlyItsTopic(t *testing.T) {
	for _, ev := range []bus.Event{
		{Topic: "node", Key: "7", Reason: "deleted:7"},
		{Topic: NodeFleetTopic, Key: "7", Reason: "renamed"},
		{Topic: NodeFleetTopic, Key: "node-uuid", Reason: string(NodeDeleted)},
		{Topic: NodeFleetTopic, Key: "0", Reason: string(NodeInserted)},
	} {
		if c, ok := NodeFleetChangeOf(ev); ok {
			t.Errorf("NodeFleetChangeOf(%+v) = %+v, want not ok", ev, c)
		}
	}
	if c, ok := NodeFleetChangeOf(bus.Event{Topic: NodeFleetTopic, Key: "42", Reason: "facts"}); !ok || c != (NodeFleetChange{42, NodeFactsChanged}) {
		t.Fatalf("facts event = %+v %v", c, ok)
	}
}

// NodeFleet reads every node's hold facts as the status reports left them,
// in enrollment order.
func TestNodeFleetReadsEveryNodesHoldFacts(t *testing.T) {
	s := newTestStore(t)
	if got, err := s.NodeFleet(context.Background()); err != nil || len(got) != 0 {
		t.Fatalf("empty fleet = %+v, %v", got, err)
	}
	a := newHoldNode(t, s, "fleet-a")
	b := newHoldNode(t, s, "fleet-b")
	rowA := a.report(proto.SysInfo{RuntimeDriver: "docker"})
	rowB := b.row()
	got, err := s.NodeFleet(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	want := []NodeHoldFacts{nodeHoldFacts(&rowA), nodeHoldFacts(&rowB)}
	if len(got) != 2 || got[0].ID >= got[1].ID {
		t.Fatalf("fleet = %+v", got)
	}
	for i := range want {
		if !got[i].CreatedAt.Equal(want[i].CreatedAt) {
			t.Fatalf("node %d created %s, want %s", i, got[i].CreatedAt, want[i].CreatedAt)
		}
		got[i].CreatedAt, want[i].CreatedAt = time.Time{}, time.Time{}
		if got[i] != want[i] {
			t.Fatalf("node %d facts = %+v, want %+v", i, got[i], want[i])
		}
	}
	if got[0].OwnedCards != 1 || got[0].GPUVendor != "nvidia" || got[1].OwnedCards != 0 {
		t.Fatalf("facts = %+v", got)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.NodeFleet(ctx); err == nil {
		t.Fatal("a cancelled read succeeded")
	}
}
