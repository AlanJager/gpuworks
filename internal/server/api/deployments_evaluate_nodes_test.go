package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// eligibleNodeFixture seeds the five-node world the eligibility cases need:
// a plain eligible node, an offline one, a vendor-mismatched one, a chip too
// old for the template predicate, and one whose labels disagree.
func eligibleNodeFixture(t *testing.T) (*apiHandlers, uint) {
	t.Helper()
	st, err := store.Open("file:eligible_nodes_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	online := func(uuid string, gpu proto.GPUInfo) {
		t.Helper()
		if _, err := st.RegisterNode(uuid, store.HashToken("token"), uuid); err != nil {
			t.Fatal(err)
		}
		if err := st.UpdateNodeStatus(uuid, proto.NodeStatus{
			Sys:  proto.SysInfo{Hostname: uuid, Arch: "amd64", GPUVendor: gpu.Vendor, Framework: "cuda", FrameworkVersion: "12.4"},
			GPUs: []proto.GPUInfo{gpu},
		}); err != nil {
			t.Fatal(err)
		}
	}
	online("eligible-node", proto.GPUInfo{Index: 0, Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90", ComputeCapability: "9.0", MemoryTotalBytes: 80 << 30, Healthy: true})
	online("chip-old-node", proto.GPUInfo{Index: 0, Name: "NVIDIA A100", Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0", MemoryTotalBytes: 40 << 30, Healthy: true})
	online("vendor-node", proto.GPUInfo{Index: 0, Name: "Ascend 910B", Vendor: "ascend", ArchID: "910B", MemoryTotalBytes: 64 << 30, Healthy: true})
	if _, err := st.RegisterNode("offline-node", store.HashToken("token"), "offline-node"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateNodeStatus("offline-node", proto.NodeStatus{
		Sys:  proto.SysInfo{Hostname: "offline-node", Arch: "amd64", GPUVendor: "nvidia"},
		GPUs: []proto.GPUInfo{{Index: 0, Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90", ComputeCapability: "9.0", MemoryTotalBytes: 80 << 30, Healthy: true}},
	}); err != nil {
		t.Fatal(err)
	}
	if err := st.SetNodeState("offline-node", store.NodeStateOffline); err != nil {
		t.Fatal(err)
	}
	online("unlabeled-node", proto.GPUInfo{Index: 0, Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90", ComputeCapability: "9.0", MemoryTotalBytes: 80 << 30, Healthy: true})
	for _, uuid := range []string{"eligible-node", "vendor-node", "chip-old-node", "offline-node"} {
		if _, err := st.SetNodeLabels(uuid, map[string]string{"pool": "inference"}); err != nil {
			t.Fatal(err)
		}
	}

	if _, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "Acme/Deploy-FP8", Revision: "main", Name: "Acme/Deploy-FP8", WeightBytes: 16 << 30}); err != nil {
		t.Fatal(err)
	}
	fixture := deploymentFlatFixture()
	fixture.ModelID = "acme/eligible"
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{fixture}); err != nil {
		t.Fatal(err)
	}
	template, err := st.InferenceModelByModelID(fixture.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	deploy.SetDefaultImage("vllm", "example/vllm:test")
	return &apiHandlers{d: Deps{Store: st}}, template.ID
}

func eligibleNodesOf(t *testing.T, h *apiHandlers, body string) map[string]struct {
	Eligible bool             `json:"eligible"`
	Reasons  []map[string]any `json:"reasons"`
} {
	t.Helper()
	rec := httptest.NewRecorder()
	h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status = %d body = %s", rec.Code, rec.Body.String())
	}
	var response struct {
		EligibleNodes []struct {
			UUID     string           `json:"uuid"`
			Name     string           `json:"name"`
			Eligible bool             `json:"eligible"`
			Reasons  []map[string]any `json:"reasons"`
		} `json:"eligible_nodes"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	out := map[string]struct {
		Eligible bool             `json:"eligible"`
		Reasons  []map[string]any `json:"reasons"`
	}{}
	for _, node := range response.EligibleNodes {
		out[node.UUID] = struct {
			Eligible bool             `json:"eligible"`
			Reasons  []map[string]any `json:"reasons"`
		}{Eligible: node.Eligible, Reasons: node.Reasons}
		if node.Name == "" {
			t.Fatalf("eligible node %s has no display name", node.UUID)
		}
	}
	return out
}

func hasReason(node struct {
	Eligible bool             `json:"eligible"`
	Reasons  []map[string]any `json:"reasons"`
}, code string) bool {
	for _, reason := range node.Reasons {
		if reason["code"] == code {
			return true
		}
	}
	return false
}

// The five cases the plan names: requirement mismatch, vendor mismatch,
// chipmatch mismatch, offline, and all-eligible. One draft answers for every node at
// once, which is exactly what the dropdown renders.
func TestEvaluateEligibleNodesFilterAMDFeatureRequirements(t *testing.T) {
	h, _ := eligibleNodeFixture(t)
	if _, err := h.d.Store.RegisterNode("amd-feature-node", store.HashToken("amd-feature-token"), "amd-feature-node"); err != nil {
		t.Fatal(err)
	}
	if err := h.d.Store.UpdateNodeStatus("amd-feature-node", proto.NodeStatus{
		Sys:  proto.SysInfo{Hostname: "amd-feature-node", OS: "linux", Arch: "amd64", GPUVendor: "amd", Framework: "rocm", RuntimeDriver: "docker"},
		GPUs: []proto.GPUInfo{{Index: 0, Name: "AMD Instinct MI300X", Vendor: "amd", ArchID: "gfx942", MemoryTotalBytes: 96 << 30, Healthy: true}},
	}); err != nil {
		t.Fatal(err)
	}
	features := []accelerator.Feature{accelerator.FeatureMultiNode, accelerator.FeaturePD, accelerator.FeatureDataParallel}
	nodes, _ := h.evaluateEligibleNodes(templateSelection{}, nil, nil, true, func(node *store.Node) map[int]planning.TargetCardRequest {
		return eligibleRequestsForTest(node, planning.TargetRequest{Features: features, CommandKnown: true})
	})
	byUUID := make(map[string]struct {
		eligible bool
		reasons  map[string]bool
	}, len(nodes))
	for _, node := range nodes {
		reasons := make(map[string]bool)
		for _, reason := range node.Reasons {
			reasons[reason.Code] = true
		}
		byUUID[node.UUID] = struct {
			eligible bool
			reasons  map[string]bool
		}{node.Eligible, reasons}
	}
	amd := byUUID["amd-feature-node"]
	// AMD runs multi-node ranks and PD; data parallelism remains refused.
	if amd.eligible || amd.reasons["multi_node_unsupported_on_node"] || amd.reasons["pd_unsupported_on_node"] || !amd.reasons["accelerator_feature_unsupported"] {
		t.Fatalf("AMD feature requirements were not explained: %+v", amd)
	}
	for _, uuid := range []string{"eligible-node", "vendor-node"} {
		if node := byUUID[uuid]; !node.eligible {
			t.Fatalf("supported vendor %s was filtered by AMD feature requirements: %+v", uuid, node)
		}
	}
}

func TestEvaluateEligibleNodesSkipsAMDFeatureWorkWithoutOwnedAMD(t *testing.T) {
	h, _ := eligibleNodeFixture(t)
	called := false
	h.evaluateEligibleNodes(templateSelection{}, nil, nil, true, func(node *store.Node) map[int]planning.TargetCardRequest {
		called = true
		return eligibleRequestsForTest(node, planning.TargetRequest{Features: []accelerator.Feature{accelerator.FeatureMultiNode}, CommandKnown: true})
	})
	if !called {
		t.Fatal("resolved launch features were not evaluated for owned cards")
	}
}

func TestEvaluateEligibleNodesCoverEveryReason(t *testing.T) {
	h, templateID := eligibleNodeFixture(t)
	body := fmt.Sprintf(`{"template_id":%d,"template_spec_name":"fp8-balanced","node_uuid":"eligible-node","target_vendor":"nvidia","node_requirements":{"pool":["inference"]}}`, templateID)
	nodes := eligibleNodesOf(t, h, body)
	if len(nodes) != 5 {
		t.Fatalf("eligible_nodes = %v, want all five seeded nodes", nodes)
	}
	if entry, ok := nodes["eligible-node"]; !ok || !entry.Eligible || len(entry.Reasons) != 0 {
		t.Fatalf("the matching labeled node must be eligible without reasons: %+v", entry)
	}
	if entry := nodes["unlabeled-node"]; entry.Eligible || !hasReason(entry, reasonRequirementsUnmet) {
		t.Fatalf("the unlabeled node must report the label mismatch: %+v", entry)
	}
	if entry := nodes["vendor-node"]; entry.Eligible || !hasReason(entry, reasonVendorMismatch) {
		t.Fatalf("the ascend node must report the vendor mismatch: %+v", entry)
	}
	if entry := nodes["chip-old-node"]; entry.Eligible || !hasReason(entry, reasonChipMismatch) {
		t.Fatalf("the sm_80 node must report the chip mismatch: %+v", entry)
	}
	if entry := nodes["offline-node"]; entry.Eligible || !hasReason(entry, reasonNodeOffline) {
		t.Fatalf("the offline node must report its state: %+v", entry)
	}
}

// A requirement on a system label is matched against what the platform
// generates for each node, and alternatives of one key are "any of": naming
// two GPU models admits nodes with either, and nothing else.
func TestEvaluateEligibleNodesMatchSystemLabels(t *testing.T) {
	h, templateID := eligibleNodeFixture(t)
	body := fmt.Sprintf(`{"template_id":%d,"template_spec_name":"fp8-balanced","placement_mode":"auto","node_requirements":{"gpuworks.ai/gpu-model":["A100","910B"]}}`, templateID)
	nodes := eligibleNodesOf(t, h, body)
	for uuid, entry := range nodes {
		named := uuid == "chip-old-node" || uuid == "vendor-node"
		if named == hasReason(entry, reasonRequirementsUnmet) {
			t.Errorf("%s: requirement unmet = %v, want %v (%+v)", uuid, !named, !named, entry)
		}
	}
}

// Requirements the evaluation cannot read leave every node ineligible with
// the reason, never every node eligible: the dropdown must not offer nodes
// the draft may exclude.
func TestEvaluateEligibleNodesWithUnreadableRequirements(t *testing.T) {
	h, templateID := eligibleNodeFixture(t)
	body := fmt.Sprintf(`{"template_id":%d,"template_spec_name":"fp8-balanced","placement_mode":"auto","node_requirements":{"gpuworks.ai/gpu_model":["A100"]}}`, templateID)
	nodes := eligibleNodesOf(t, h, body)
	if len(nodes) != 5 {
		t.Fatalf("eligible_nodes = %v", nodes)
	}
	for uuid, entry := range nodes {
		if entry.Eligible || !hasReason(entry, reasonRequirementsInvalid) {
			t.Errorf("%s: %+v, want ineligible for unreadable requirements", uuid, entry)
		}
	}
}

// Without a template predicate every healthy nvidia node is eligible for an
// explicit deployment; the request's own constraints still apply.
func TestEvaluateEligibleNodesWithoutTemplate(t *testing.T) {
	st, err := store.Open("file:eligible_nodes_plain?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/plain", Revision: "main", Name: "plain", WeightBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterNode("plain-node", store.HashToken("token"), "plain-node"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateNodeStatus("plain-node", proto.NodeStatus{
		Sys:  proto.SysInfo{Hostname: "plain-node", Arch: "amd64", GPUVendor: "nvidia", Framework: "cuda", FrameworkVersion: "12.4"},
		GPUs: []proto.GPUInfo{{Index: 0, Name: "NVIDIA GPU", Vendor: "nvidia", ArchID: "sm_80", ComputeCapability: "8.0", MemoryTotalBytes: 16 << 30, Healthy: true}},
	}); err != nil {
		t.Fatal(err)
	}
	deploy.SetDefaultImage("vllm", "example/vllm:test")
	h := &apiHandlers{d: Deps{Store: st}}
	nodes := eligibleNodesOf(t, h, fmt.Sprintf(`{"model_id":%d,"node_uuid":"plain-node","engine":"vllm"}`, model.ID))
	if entry, ok := nodes["plain-node"]; !ok || !entry.Eligible {
		t.Fatalf("a plain online node must be eligible: %+v", entry)
	}
}

func eligibleRequestsForTest(node *store.Node, request planning.TargetRequest) map[int]planning.TargetCardRequest {
	out := map[int]planning.TargetCardRequest{}
	for _, gpu := range node.OwnedGPUs() {
		out[gpu.Index] = planning.TargetCardRequest{Request: request}
	}
	return out
}
