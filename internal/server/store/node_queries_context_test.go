package store

import (
	"context"
	"errors"
	"testing"
)

func TestServerDialNodesContextPreservesQueryAndCancellation(t *testing.T) {
	s := newTestStore(t)
	empty, err := s.ServerDialNodesContext(t.Context())
	if err != nil || len(empty) != 0 {
		t.Fatalf("empty nodes: count=%d err=%v", len(empty), err)
	}
	n, err := s.RegisterNode("context-dial", HashToken("fixture-token"), "context-dial", "server_dial")
	if err != nil {
		t.Fatal(err)
	}
	if err := s.SetNodeServerDialAddress(n.UUID, "127.0.0.1:19090"); err != nil {
		t.Fatal(err)
	}
	got, err := s.ServerDialNodesContext(t.Context())
	if err != nil || len(got) != 1 || got[0].ID != n.ID {
		t.Fatalf("context query count=%d err=%v", len(got), err)
	}
	legacy, err := s.ServerDialNodes()
	if err != nil || len(legacy) != 1 || legacy[0].ID != got[0].ID {
		t.Fatal("legacy query changed")
	}
	ctx, cancel := context.WithCancel(t.Context())
	cancel()
	if _, err := s.ServerDialNodesContext(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("canceled query error=%v", err)
	}
	// Failed/canceled reads cannot change eligibility or poison later reads.
	after, err := s.ServerDialNodesContext(t.Context())
	if err != nil || len(after) != 1 || after[0].ID != n.ID {
		t.Fatalf("read after cancellation count=%d err=%v", len(after), err)
	}
	if err := s.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := s.ServerDialNodesContext(t.Context()); err == nil {
		t.Fatal("closed database query falsely succeeded")
	}
}
