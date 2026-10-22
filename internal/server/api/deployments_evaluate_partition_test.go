package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// partitionPoolFixture registers one node per vendor. A node given a backend
// reports that backend's assets and a passed probe, which is what makes its
// dGPU capability ready.
func partitionPoolFixture(t *testing.T, nodes map[string]string) (*apiHandlers, uint) {
	t.Helper()
	st, err := store.Open("file:"+strings.ReplaceAll(t.Name(), "/", "_")+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	for uuid, backend := range nodes {
		vendor, arch, framework := "nvidia", "sm_80", "cuda"
		if backend == accelerator.BackendAscendLogical {
			vendor, arch, framework = "ascend", "910B4", "cann"
		}
		if _, err := st.RegisterNode(uuid, store.HashToken("token"), uuid); err != nil {
			t.Fatal(err)
		}
		sys := proto.SysInfo{Hostname: uuid, Arch: "amd64", GPUVendor: vendor, Framework: framework, FrameworkVersion: "8.5.1", RuntimeDriver: "docker"}
		if backend != "" {
			sys.PartitionAssets = []proto.AssetReport{{Backend: backend, Root: "/assets", Files: []string{"libvgpu.so", "libvruntime.so"}}}
			sys.PartitionProbes = []proto.PartitionProbeResult{{Backend: backend, GPUIndex: new(int), Status: proto.PartitionProbePassed}}
			sys.PartitionSafetyProtocol = 1
		}
		if err := st.UpdateNodeStatus(uuid, proto.NodeStatus{Sys: sys, GPUs: []proto.GPUInfo{{
			Index: 0, Name: "accelerator", Vendor: vendor, ArchID: arch, MemoryTotalBytes: 64 << 30, Healthy: true,
		}}}); err != nil {
			t.Fatal(err)
		}
	}
	deploy.SetDefaultImage("vllm", "example/vllm:test")
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/partition-pool", Revision: "main", Name: "partition-pool", WeightBytes: 4 << 30})
	if err != nil {
		t.Fatal(err)
	}
	return &apiHandlers{d: Deps{Store: st}}, model.ID
}

func evaluateWire(t *testing.T, h *apiHandlers, body string) map[string]json.RawMessage {
	t.Helper()
	rec := httptest.NewRecorder()
	h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil {
		t.Fatal(err)
	}
	return wire
}

// An automatic draft has no node of its own until a placement fits, and a
// placement that only fits as a dGPU slice never fits before the quota is set.
// The pool's dGPU backend therefore rides on every evaluation, so the form can
// offer the quota on a cluster whose cards are all partly taken.
func TestEvaluateReportsThePoolPartitionCapability(t *testing.T) {
	t.Run("ready in the pool", func(t *testing.T) {
		h, modelID := partitionPoolFixture(t, map[string]string{"pool-nvidia": accelerator.BackendNVIDIALogical})
		wire := evaluateWire(t, h, fmt.Sprintf(`{"model_id":%d,"engine":"vllm","placement_mode":"auto","target_vendor":"nvidia"}`, modelID))
		var capability struct {
			Backend string `json:"backend"`
			Vendor  string `json:"vendor"`
			Status  string `json:"status"`
		}
		if err := json.Unmarshal(wire["pool_partition_capability"], &capability); err != nil {
			t.Fatalf("pool_partition_capability = %s: %v", wire["pool_partition_capability"], err)
		}
		if capability.Backend != accelerator.BackendNVIDIALogical || capability.Vendor != "nvidia" || capability.Status != "ready" {
			t.Fatalf("pool_partition_capability = %+v", capability)
		}
	})
	t.Run("absent for a draft that names its node", func(t *testing.T) {
		h, modelID := partitionPoolFixture(t, map[string]string{"pool-named": accelerator.BackendNVIDIALogical})
		wire := evaluateWire(t, h, fmt.Sprintf(`{"model_id":%d,"engine":"vllm","node_uuid":"pool-named","target_vendor":"nvidia"}`, modelID))
		if raw, ok := wire["pool_partition_capability"]; ok {
			t.Fatalf("pool_partition_capability = %s, want the key absent", raw)
		}
	})
	t.Run("absent without one", func(t *testing.T) {
		h, modelID := partitionPoolFixture(t, map[string]string{"pool-plain": ""})
		wire := evaluateWire(t, h, fmt.Sprintf(`{"model_id":%d,"engine":"vllm","placement_mode":"auto","target_vendor":"nvidia"}`, modelID))
		if raw, ok := wire["pool_partition_capability"]; ok {
			t.Fatalf("pool_partition_capability = %s, want the key absent", raw)
		}
	})
}

// An automatic quota names no backend; each candidate resolves its own. The
// historical NVIDIA default answered partition_unavailable on every Ascend
// node, so an automatic dGPU deployment on Ascend never found a node.
func TestAutomaticQuotaPlacesOnTheNodesOwnBackend(t *testing.T) {
	h, modelID := partitionPoolFixture(t, map[string]string{"pool-ascend": accelerator.BackendAscendLogical})
	wire := evaluateWire(t, h, fmt.Sprintf(`{"model_id":%d,"engine":"vllm","placement_mode":"auto","target_vendor":"ascend","vram_limit_gib":16}`, modelID))
	var selected struct {
		UUID string `json:"uuid"`
	}
	if err := json.Unmarshal(wire["selected_node"], &selected); err != nil || selected.UUID != "pool-ascend" {
		t.Fatalf("selected_node = %s (candidates %s), want pool-ascend", wire["selected_node"], wire["candidates"])
	}
}

// TestEvaluateAutoPartitionPoolFixture pins the answer the deploy form renders
// when every card is taken: a dGPU-ready node whose only card a whole-GPU
// instance holds. No whole-card placement fits, so there is no selected node,
// and pool_partition_capability is what lets the form offer the quota anyway.
// Regenerate with GPUWORKS_UPDATE_UI_FIXTURES=1.
func TestEvaluateAutoPartitionPoolFixture(t *testing.T) {
	h, modelID := partitionPoolFixture(t, map[string]string{"pool-busy": accelerator.BackendNVIDIALogical})
	st := h.d.Store
	node, err := st.NodeWithGPUs("pool-busy")
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&store.Deployment{Name: "whole-card", ModelID: modelID, NodeID: node.ID, Engine: "vllm", Replicas: 1})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.CreateInstance(&store.Instance{UUID: "whole-card-0", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, GPUIndexes: `[0]`}); err != nil {
		t.Fatal(err)
	}
	update := os.Getenv(evaluateFixturesEnv) != ""
	checkEvaluateFixture(t, h, "auto_partition_pool", fmt.Sprintf(`{"model_id":%d,"engine":"vllm","placement_mode":"auto","target_vendor":"nvidia"}`, modelID), update)
	if update {
		t.Fatalf("regenerated the fixture under %s; rerun without %s", evaluateFixturesDir, evaluateFixturesEnv)
	}
}
