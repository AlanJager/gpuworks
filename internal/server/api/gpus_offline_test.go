package api

import (
	"encoding/json"
	"net/http"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestGPUListExcludesDisconnectedNodesFromAllocatableResources(t *testing.T) {
	rig := newTokenRig(t)
	t.Cleanup(func() { _ = rig.st.Close() })
	for _, state := range []string{store.NodeStateOnline, store.NodeStateOffline, store.NodeStateConnecting} {
		node, err := rig.st.RegisterNode("gpu-"+state, store.HashToken(state), "gpu-"+state)
		if err != nil {
			t.Fatal(err)
		}
		if err := rig.st.UpdateNodeStatus(node.UUID, proto.NodeStatus{GPUs: []proto.GPUInfo{{Index: 0, UUID: "card-" + state, Name: "NVIDIA H100", Vendor: "nvidia", Healthy: true, MemoryTotalBytes: 80 << 30}}}); err != nil {
			t.Fatal(err)
		}
		if err := rig.st.SetNodeState(node.UUID, state); err != nil {
			t.Fatal(err)
		}
	}
	for _, path := range []string{"/api/v1/gpus", "/api/v1/gpus?page=1&page_size=20"} {
		rec := rig.do(rig.session, http.MethodGet, path, "")
		var inventory dto.GPUListResponse
		if err := json.Unmarshal(rec.Body.Bytes(), &inventory); err != nil || rec.Code != http.StatusOK {
			t.Fatalf("GET %s: %d %s, err=%v", path, rec.Code, rec.Body, err)
		}
		if inventory.Overview.Total != 3 || inventory.Overview.Allocation.Allocatable != 1 || inventory.Overview.Allocation.Unknown != 2 {
			t.Errorf("GET %s overview=%+v; want 3 total, 1 allocatable, 2 unknown", path, inventory.Overview)
		}
		for _, gpu := range inventory.Items {
			if gpu.NodeState == store.NodeStateOnline {
				if gpu.AllocationState != "available" || gpu.AvailableBytes != 80<<30 {
					t.Errorf("online GPU=%+v", gpu)
				}
			} else if gpu.AllocationState != "unknown" || gpu.AvailableBytes != 0 || gpu.HealthStatus != "OK" {
				// "unknown", not "unusable": nothing is known to be wrong
				// with the card, only that nobody can reach the node to ask.
				t.Errorf("a card on an unreachable node must read as unknown with no allocatable memory: %+v", gpu)
			}
		}
	}
	for state, count := range map[string]int{"unknown": 2, "available": 1} {
		rec := rig.do(rig.session, http.MethodGet, "/api/v1/gpus?page=1&allocation_state="+state, "")
		var page dto.GPUPage
		if err := json.Unmarshal(rec.Body.Bytes(), &page); err != nil || rec.Code != http.StatusOK || page.Total != count || len(page.Items) != count {
			t.Fatalf("allocation filter %s: %d %s, err=%v", state, rec.Code, rec.Body, err)
		}
		for _, gpu := range page.Items {
			if gpu.AllocationState != state {
				t.Errorf("filter %s returned %s", state, gpu.AllocationState)
			}
		}
	}
}
