package store

import (
	"context"
	"errors"
	"testing"
)

// NodeNamesByID names each node that exists by its display name; missing ids
// are absent, duplicates count once, and no ids is an empty answer.
func TestNodeNamesByIDNamesTheNodesThatExist(t *testing.T) {
	s := newTestStore(t)
	left, _, ascend := identityFleet(t, s)
	unnamed, _, err := s.CreateNodePending("")
	if err != nil {
		t.Fatal(err)
	}
	gone, _, err := s.CreateNodePending("gone")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.DeleteNode(gone.UUID); err != nil {
		t.Fatal(err)
	}

	got, err := s.NodeNamesByID(context.Background(), []uint{ascend.ID, left.ID, left.ID, unnamed.ID, gone.ID, 0, 999999})
	if err != nil {
		t.Fatal(err)
	}
	want := map[uint]string{left.ID: "left", ascend.ID: "ascend", unnamed.ID: unnamed.UUID[:8]}
	if len(got) != len(want) {
		t.Fatalf("names = %v, want %v", got, want)
	}
	for id, name := range want {
		if got[id] != name {
			t.Fatalf("names = %v, want %v", got, want)
		}
	}

	for _, ids := range [][]uint{nil, {}, {0}} {
		empty, err := s.NodeNamesByID(context.Background(), ids)
		if err != nil || empty == nil || len(empty) != 0 {
			t.Fatalf("ids %v = %#v, %v; want an empty map", ids, empty, err)
		}
	}
}

func TestNodeNamesByIDRefusesTooManyIDs(t *testing.T) {
	s := newTestStore(t)
	ids := make([]uint, MaxNodeReadBatch+1)
	for i := range ids {
		ids[i] = uint(i + 1)
	}
	if got, err := s.NodeNamesByID(context.Background(), ids); !errors.Is(err, ErrNodeReadBatch) || got != nil {
		t.Fatalf("%d ids = %v, %v; want ErrNodeReadBatch", len(ids), got, err)
	}
	// Exactly the limit, with duplicates beyond it, is accepted.
	if _, err := s.NodeNamesByID(context.Background(), append(ids[:MaxNodeReadBatch:MaxNodeReadBatch], 1, 2)); err != nil {
		t.Fatalf("%d distinct ids = %v", MaxNodeReadBatch, err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := s.NodeNamesByID(ctx, []uint{1}); err == nil {
		t.Fatal("a cancelled context must fail the read")
	}
}
