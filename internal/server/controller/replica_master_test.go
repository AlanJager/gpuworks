package controller

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A replica rendezvous on its own leader: the stored master address only for
// the first replica's leader it was stored for, the leader's parameter
// address otherwise, and no guess from another replica's when the leader has
// none.
func TestReplicaMasterIsTheReplicasOwnLeader(t *testing.T) {
	first := &store.Node{ID: 1, UUID: "first"}
	other := &store.Node{ID: 3, UUID: "other"}
	group := netplan.Group{Ranks: map[uint]netplan.Resolved{1: {IP: "10.0.0.1"}, 3: {IP: "10.0.0.3"}}}
	dep := &store.Deployment{ID: 7, NodeID: 1, MasterAddr: "192.168.9.1"}

	if got, err := replicaMaster(dep, first, group); err != nil || got != "192.168.9.1" {
		t.Fatalf("first replica master = %q %v, want the stored override", got, err)
	}
	if got, err := replicaMaster(dep, other, group); err != nil || got != "10.0.0.3" {
		t.Fatalf("another replica's master = %q %v, want its own leader", got, err)
	}
	// The list now leads the first replica from node 3; the store cleared the
	// address stored for node 1.
	moved := &store.Deployment{ID: 7, NodeID: 3}
	if got, err := replicaMaster(moved, other, group); err != nil || got != "10.0.0.3" {
		t.Fatalf("moved first replica's master = %q %v, want its new leader", got, err)
	}
	if got, err := replicaMaster(dep, other, netplan.Group{Ranks: map[uint]netplan.Resolved{}}); err == nil {
		t.Fatalf("a leader without an address got master %q, want an error", got)
	}
}
