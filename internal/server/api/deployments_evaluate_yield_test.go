package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/servicelevel"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The deploy form's previews say why a draft does not fit as the controller
// would record it: a draft that shares cards by yield policy, on a card an
// awake deployment that gives way to it holds, waits for that one to yield
// (waiting_for_yield), in the manual plan and in the automatic candidates
// alike; a draft that shares with no one reads as the card being in use.
func TestEvaluateSaysADraftWaitsForTheHolderToYield(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("yield-node", store.HashToken("token"), "yield-node")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateNodeStatus("yield-node", proto.NodeStatus{
		Sys:  proto.SysInfo{Hostname: "yield-node", Arch: "amd64", GPUVendor: "nvidia", Framework: "cuda", FrameworkVersion: "12.4"},
		GPUs: []proto.GPUInfo{{Index: 0, Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90", ComputeCapability: "9.0", MemoryTotalBytes: 80 << 30, Healthy: true}},
	}); err != nil {
		t.Fatal(err)
	}
	deploy.SetDefaultImage("vllm", "example/vllm:test")
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/yield", Revision: "main", Name: "yield", WeightBytes: 20 << 30})
	if err != nil {
		t.Fatal(err)
	}
	batch, err := st.CreateDeployment(&store.Deployment{Name: "batch", ModelID: model.ID, NodeID: node.ID, ServedName: "batch", Engine: "vllm", Sleepable: true, State: store.DeployActive, Replicas: 1})
	if err != nil {
		t.Fatal(err)
	}
	anytime, _ := servicelevel.Parse(servicelevel.Anytime, servicelevel.Overrides{})
	if err := st.SetDeploymentYieldPolicy(batch.ID, anytime); err != nil {
		t.Fatal(err)
	}
	spec, _ := json.Marshal(proto.InstanceDesiredState{UUID: "batch-1", DesiredState: "running", Sleepable: true})
	if _, err := st.CreateInstance(&store.Instance{UUID: "batch-1", DeploymentID: batch.ID, NodeID: node.ID, State: proto.InstRunning, GPUIndexes: "[0]", NNodes: 1, APIPort: 30001, DesiredStateJSON: string(spec)}); err != nil {
		t.Fatal(err)
	}
	h := &apiHandlers{d: Deps{Store: st}}
	evaluate := func(body string) map[string]any {
		t.Helper()
		rec := httptest.NewRecorder()
		h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("evaluate status = %d body = %s", rec.Code, rec.Body.String())
		}
		var out map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	planCode := func(out map[string]any) string {
		plan, _ := out["plan"].(map[string]any)
		message, _ := plan["message"].(map[string]any)
		code, _ := message["code"].(string)
		return code
	}

	manual := `{"model_id":%d,"engine":"vllm","node_uuid":"yield-node","sleepable":true%s}`
	idle := `,"yield_policy":{"policy":"idle"}`
	if got := planCode(evaluate(fmt.Sprintf(manual, model.ID, idle))); got != "waiting_for_yield" {
		t.Fatalf("manual plan of an idle draft = %q, want waiting_for_yield", got)
	}
	if got := planCode(evaluate(fmt.Sprintf(manual, model.ID, ""))); got == "waiting_for_yield" || got == "" {
		t.Fatalf("manual plan of a never draft = %q, want the scheduler's own refusal", got)
	}

	auto := evaluate(fmt.Sprintf(`{"model_id":%d,"engine":"vllm","placement_mode":"auto","target_vendor":"nvidia","sleepable":true%s}`, model.ID, idle))
	candidates, _ := auto["candidates"].([]any)
	var reason string
	for _, raw := range candidates {
		candidate, _ := raw.(map[string]any)
		if candidate["node_uuid"] == "yield-node" {
			r, _ := candidate["reason"].(map[string]any)
			reason, _ = r["code"].(string)
		}
	}
	if reason != "waiting_for_yield" {
		t.Fatalf("automatic candidate reason = %q, want waiting_for_yield: %v", reason, candidates)
	}
}
