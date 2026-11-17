package store

import (
	"context"
	"errors"
	"testing"
)

// The inventory pages through every node in enrollment order with its
// display name, its own cards (models, indexes, whether each has a
// serial) and whether its host UUID is known; never a raw serial.
func TestNodeInventoryPagesInEnrollmentOrder(t *testing.T) {
	s := newTestStore(t)
	left, right, ascend := identityFleet(t, s)
	pending, _, err := s.CreateNodePending("")
	if err != nil {
		t.Fatal(err)
	}

	first, err := s.NodeInventory(context.Background(), 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(first.Nodes) != 2 || first.Nodes[0].ID != left.ID || first.Nodes[1].ID != right.ID || first.Next != right.ID {
		t.Fatalf("first page = %+v", first)
	}
	l := first.Nodes[0]
	if l.UUID != left.UUID || l.Name != "left" || !l.HostReported || l.CreatedAt.IsZero() {
		t.Fatalf("left node = %+v", l)
	}
	if len(l.Cards) != 1 || l.Cards[0] != (NodeInventoryCard{Index: 0, Model: "H800", Serial: true}) {
		t.Fatalf("left cards = %+v, want only the card it owns", l.Cards)
	}

	second, err := s.NodeInventory(context.Background(), first.Next, 2)
	if err != nil {
		t.Fatal(err)
	}
	if len(second.Nodes) != 2 || second.Nodes[0].ID != ascend.ID || second.Nodes[1].ID != pending.ID || second.Next != 0 {
		t.Fatalf("second page = %+v, want the last two and no next cursor", second)
	}
	a := second.Nodes[0]
	if a.HostReported || len(a.Cards) != 2 || a.Cards[0] != (NodeInventoryCard{Index: 0, Model: "H800"}) || a.Cards[1].Index != 1 || a.Cards[1].Serial {
		t.Fatalf("ascend node = %+v", a)
	}
	p := second.Nodes[1]
	if p.Name != pending.UUID[:8] || p.HostReported || p.Cards == nil || len(p.Cards) != 0 {
		t.Fatalf("unnamed node without cards = %+v", p)
	}

	// A node deleted between pages neither shifts nor repeats the rest.
	if err := s.DeleteNode(ascend.UUID); err != nil {
		t.Fatal(err)
	}
	after, err := s.NodeInventory(context.Background(), first.Next, 2)
	if err != nil || len(after.Nodes) != 1 || after.Nodes[0].ID != pending.ID || after.Next != 0 {
		t.Fatalf("after a delete = %+v, %v", after, err)
	}
	empty, err := s.NodeInventory(context.Background(), pending.ID, MaxNodeReadBatch)
	if err != nil || empty.Nodes == nil || len(empty.Nodes) != 0 || empty.Next != 0 {
		t.Fatalf("past the end = %#v, %v; want an empty page", empty, err)
	}
	// A page exactly as long as what is left has no next cursor.
	exact, err := s.NodeInventory(context.Background(), 0, 3)
	if err != nil || len(exact.Nodes) != 3 || exact.Next != 0 {
		t.Fatalf("exact page = %+v, %v", exact, err)
	}
}

func TestNodeInventoryRefusesOutOfBoundsLimits(t *testing.T) {
	s := newTestStore(t)
	for _, limit := range []int{0, -1, MaxNodeReadBatch + 1} {
		if page, err := s.NodeInventory(context.Background(), 0, limit); !errors.Is(err, ErrNodeReadBatch) || page.Nodes != nil {
			t.Errorf("limit %d = %+v, %v; want ErrNodeReadBatch", limit, page, err)
		}
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.NodeInventory(ctx, 0, 10); err == nil {
		t.Fatal("a cancelled context must fail the read")
	}
}
