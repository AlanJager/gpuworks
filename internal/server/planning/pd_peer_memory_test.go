package planning

import (
	"errors"
	"slices"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/pdmode"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A PD mode over RDMA places only on nodes whose peer-memory module is
// loaded; when that leaves none, the error names the module and its fix.
func TestChoosePDUnitsLeavesOutNodesWithoutPeerMemory(t *testing.T) {
	mode, _ := pdmode.Lookup("vllm-ppu-mooncake")
	facts := choiceFacts(map[uint]int{1: 8, 2: 8, 3: 2}, map[uint]netplan.Network{1: rdma, 2: rdma, 3: rdma})
	// n-1: missing, n-2: an agent that does not report it, n-3: loaded.
	for i, state := range []string{proto.PeerMemoryMissing, "", proto.PeerMemoryLoaded} {
		facts.Nodes[i].PeerMemory = state
	}
	facts.PeerMemory = mode.PeerMemory
	roles := []PDRoleNeed{{Role: "prefill", Units: 1, GPUs: 1}, {Role: "decode", Units: 1, GPUs: 1}}
	units, _, ok := ChoosePDUnits(facts, roles, nil, store.PlacementSpread, acceptPD)
	if !ok || !slices.Equal(pdNodes(units, "prefill"), []string{"n-3"}) || !slices.Equal(pdNodes(units, "decode"), []string{"n-3"}) {
		t.Fatalf("units = %v %v, want both on n-3, the only node with the module", units, ok)
	}

	facts.Nodes[2].PeerMemory = proto.PeerMemoryMissing
	_, counts, ok := ChoosePDUnits(facts, roles, nil, store.PlacementSpread, acceptPD)
	if ok || counts.NoPeerMemory != 3 {
		t.Fatalf("ok=%v counts=%+v, want every node left out for the module", ok, counts)
	}
	err := PDPlacementNone(mode, "ppu", counts)
	var product *messages.ProductError
	if !errors.As(err, &product) || product.Code() != messages.ErrorPDPeerMemoryNone {
		t.Fatalf("err = %v", err)
	}
	params := product.Params()
	if params["module"] != "alixpu_peermem" || params["no_peer_memory"] != 3 || params["fix"] != "modprobe alixpu_peermem && echo alixpu_peermem > /etc/modules-load.d/alixpu_peermem.conf" {
		t.Fatalf("params = %v", params)
	}
	// A mode that does not need the module ignores it.
	facts.PeerMemory = false
	if _, _, ok := ChoosePDUnits(facts, roles, nil, store.PlacementSpread, acceptPD); !ok {
		t.Fatal("a mode without RDMA was kept off nodes without the module")
	}
	nixl, _ := pdmode.Lookup("vllm-nixl")
	if err := PDPlacementNone(nixl, "nvidia", UnitSuggestCounts{Nodes: 3, NoPeerMemory: 3}); productCode(err) == messages.ErrorPDPeerMemoryNone {
		t.Fatalf("vllm-nixl: %v", err)
	}
}

// A manual placement on a node without the module is refused with the node,
// the module and the fix.
func TestValidatePDNodesPeerMemory(t *testing.T) {
	loaded := pdNode("p1", store.NodeStateOnline, "PPU-ZW810E", "PPU-ZW810E")
	loaded.PeerMemory = proto.PeerMemoryLoaded
	missing := pdNode("p2", store.NodeStateOnline, "PPU-ZW810E", "PPU-ZW810E")
	missing.PeerMemory = proto.PeerMemoryMissing
	nodes := map[string]*store.Node{"p1": loaded, "p2": missing}
	check := func(text string) error {
		pd := parsePDText(t, text)
		mode, err := ValidatePD(pd, "sglang")
		if err != nil {
			t.Fatalf("structural: %v", err)
		}
		_, err = ValidatePDNodes(pd, mode, nodes, true)
		return err
	}
	if err := check(`{"pd":{"mode":"sglang-ppu-mooncake","roles":{"prefill":{"units":[{"node_uuid":"p1","gpus":[0]}]},"decode":{"units":[{"node_uuid":"p1","gpus":[1]}]}}}}`); err != nil {
		t.Fatalf("loaded node: %v", err)
	}
	err := check(`{"pd":{"mode":"sglang-ppu-mooncake","roles":{"prefill":{"units":[{"node_uuid":"p1"}]},"decode":{"units":[{"node_uuid":"p2"}]}}}}`)
	var product *messages.ProductError
	if !errors.As(err, &product) || product.Code() != messages.ErrorPDPeerMemoryMissing {
		t.Fatalf("err = %v", err)
	}
	if p := product.Params(); p["node"] != "p2" || p["role"] != "decode" || p["module"] != "alixpu_peermem" || p["state"] != proto.PeerMemoryMissing {
		t.Fatalf("params = %v", p)
	}
	// The same-host IPC mode does not need the module.
	if err := check(`{"pd":{"mode":"sglang-ppu-ipc","roles":{"prefill":{"units":[{"node_uuid":"p2","gpus":[0]}]},"decode":{"units":[{"node_uuid":"p2","gpus":[1]}]}}}}`); err != nil {
		t.Fatalf("ipc mode: %v", err)
	}
}
