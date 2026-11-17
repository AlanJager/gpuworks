package hub

import (
	"net"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
)

// The hello's host system UUID lands on the node, normalized. An old or
// non-root agent (no value) and a firmware placeholder keep what an earlier
// agent reported, a reconnect with the same value writes nothing, and a
// different machine's value replaces it.
func TestHandshakeRecordsTheHostSystemUUID(t *testing.T) {
	h := newTestHub(t)
	uuid, _ := newTestNode(t, h.store)
	sub := h.store.Bus.Subscribe(64, "node")
	defer sub.Close()
	ctrl, peer := net.Pipe()
	s := newSession(h, nil, ctrl, "10.0.0.1:5000")
	t.Cleanup(func() { s.Close(); _ = peer.Close() })

	handshake := func(id string) {
		t.Helper()
		n, err := h.store.NodeByUUID(uuid)
		if err != nil {
			t.Fatal(err)
		}
		s.recordHandshakeFacts(n, proto.Hello{Version: "v1", HostSystemUUID: id})
	}
	stored := func() string {
		t.Helper()
		n, err := h.store.NodeByUUID(uuid)
		if err != nil {
			t.Fatal(err)
		}
		return n.HostSystemUUID
	}
	changes := func() int {
		t.Helper()
		count := 0
		for {
			select {
			case ev := <-sub.C():
				if ev.Reason == "host_system_uuid" {
					count++
				}
			case <-time.After(100 * time.Millisecond):
				return count
			}
		}
	}

	handshake("4C4C4544-0042-3510-8051-B4C04F4E3432")
	if got := stored(); got != "4c4c4544-0042-3510-8051-b4c04f4e3432" {
		t.Fatalf("stored = %q", got)
	}
	if n := changes(); n != 1 {
		t.Fatalf("first report published %d changes, want 1", n)
	}
	for _, id := range []string{"", "03000200-0400-0500-0006-000700080009", "4c4c4544-0042-3510-8051-b4c04f4e3432"} {
		handshake(id)
		if got := stored(); got != "4c4c4544-0042-3510-8051-b4c04f4e3432" {
			t.Fatalf("after hello %q stored = %q", id, got)
		}
	}
	if n := changes(); n != 0 {
		t.Fatalf("reconnects without a new value published %d changes", n)
	}
	handshake("ec2a1b2c-3d4e-5f60-7182-93a4b5c6d7e8")
	if got := stored(); got != "ec2a1b2c-3d4e-5f60-7182-93a4b5c6d7e8" {
		t.Fatalf("moved node stored = %q", got)
	}
}
