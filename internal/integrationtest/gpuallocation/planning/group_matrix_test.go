package planning_test

import (
	"fmt"
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/integrationtest/gpuallocation/internal/fixtures"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/parallelism"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/scheduler"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Group suggestion is only a capacity hint. Its trial calls the real card
// scheduler so a group with enough cards but too little memory is rejected.
// The fallback must preserve network/architecture and explicit rank pins.
func TestReplicaGroupByStrategyConstraintsAndTrial(t *testing.T) {
	st := fixtures.NewStore(t) // No fixture node can overlap the synthetic identities.
	for _, strategy := range []store.PlacementStrategy{store.PlacementSpread, store.PlacementBinpack} {
		for _, condition := range []string{"fits", "memory-short", "architecture-split", "networks-split", "pending-claim"} {
			for _, pinned := range []bool{false, true} {
				t.Run(fmt.Sprintf("%s/%s/pinned-%v", strategy, condition, pinned), func(t *testing.T) {
					facts := planning.UnitSuggestFacts{Ranks: 2, PerRank: 2}
					for id := uint(1); id <= 4; id++ {
						node := store.Node{ID: id, UUID: fmt.Sprintf("n%d", id), GPUVendor: "nvidia", Arch: "amd64", OS: "linux"}
						for index := 0; index < 2; index++ {
							node.GPUs = append(node.GPUs, store.GPU{Index: index, UUID: fmt.Sprintf("n%d-g%d", id, index), Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0", MemoryTotal: 64 * gib})
						}
						network := netplan.Network{ID: 1, Type: netplan.TypeHostRDMA}
						if id > 2 {
							network = netplan.Network{ID: 2, Type: netplan.TypeHostEthernet}
						}
						if condition == "networks-split" {
							network.ID = id
						}
						if id == 1 && condition == "memory-short" {
							node.GPUs[0].MemoryTotal = 8 * gib
						}
						if id == 2 && condition == "architecture-split" {
							for i := range node.GPUs {
								node.GPUs[i].ArchID = "sm_90"
								node.GPUs[i].ComputeCapability = "9.0"
							}
						}
						facts.Nodes = append(facts.Nodes, node)
						facts.Facts.Networks = append(facts.Facts.Networks, netplan.Resolved{NodeID: id, Role: netplan.RoleCompute, Network: network, IP: fmt.Sprintf("192.0.2.%d", id), Source: netplan.SourceNode})
					}
					if condition == "pending-claim" {
						facts.Claimed = map[string]int{"n1": 1}
					}
					var fixed []string
					if pinned {
						fixed = []string{"n1"}
					}
					trials := 0
					nodes, _, ok := planning.ChooseReplicaNodes(facts, strategy, fixed, func(nodes []*store.Node) int {
						trials++
						for rank, node := range nodes {
							request := scheduler.Request{WeightBytes: 32 * gib, ReserveBytes: 2 * gib, GPUMemUtil: 0.9, Parallel: parallelism.Spec{TP: 4, NNodes: 2}, HeadCount: 8}
							if !scheduler.Schedule(request, planning.NodeSnapshot(st, node, true, "")).OK {
								return rank
							}
						}
						return -1
					})
					want := []string{"n1", "n2"}
					if condition != "fits" {
						want = []string{"n3", "n4"}
					}
					if condition == "networks-split" || (pinned && condition != "fits") {
						want = nil
					}
					var got []string
					for _, node := range nodes {
						got = append(got, node.UUID)
					}
					if ok != (want != nil) || !slices.Equal(got, want) {
						t.Fatalf("group=%v ok=%v, want %v", got, ok, want)
					}
					if condition == "memory-short" && !pinned && trials < 2 {
						t.Fatalf("did not trial the memory-short group before fallback: %d", trials)
					}
				})
			}
		}
	}
}
