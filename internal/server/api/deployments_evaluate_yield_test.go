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
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/server/yieldpolicy"
)

// yieldEvaluate is a node of one 80 GiB card held awake by batch (yield
// policy anytime) and an evaluate call against it, for drafts of the model.
type yieldEvaluate struct {
	st       *store.Store
	model    *store.Model
	batch    *store.Deployment
	evaluate func(body string) map[string]any
	raw      func(body string) []byte
}

func newYieldEvaluate(t *testing.T) *yieldEvaluate {
	t.Helper()
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
	anytime, _ := yieldpolicy.Parse(yieldpolicy.Anytime, yieldpolicy.Overrides{})
	if err := st.SetDeploymentYieldPolicy(batch.ID, anytime); err != nil {
		t.Fatal(err)
	}
	spec, _ := json.Marshal(proto.InstanceDesiredState{UUID: "batch-1", DesiredState: "running", Sleepable: true})
	if _, err := st.CreateInstance(&store.Instance{UUID: "batch-1", DeploymentID: batch.ID, NodeID: node.ID, State: proto.InstRunning, GPUIndexes: "[0]", NNodes: 1, APIPort: 30001, DesiredStateJSON: string(spec)}); err != nil {
		t.Fatal(err)
	}
	h := &apiHandlers{d: Deps{Store: st}}
	raw := func(body string) []byte {
		t.Helper()
		rec := httptest.NewRecorder()
		h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("evaluate status = %d body = %s", rec.Code, rec.Body.String())
		}
		return rec.Body.Bytes()
	}
	evaluate := func(body string) map[string]any {
		t.Helper()
		var out map[string]any
		if err := json.Unmarshal(raw(body), &out); err != nil {
			t.Fatal(err)
		}
		return out
	}
	return &yieldEvaluate{st: st, model: model, batch: batch, evaluate: evaluate, raw: raw}
}

// The deploy form's previews say why a draft does not fit as the controller
// would record it: a draft that shares cards by yield policy, on a card an
// awake deployment that gives way to it holds, waits for that one to yield
// (waiting_for_yield), in the manual plan and in the automatic candidates
// alike; a draft that never yields does not wait for it, and says so
// (does_not_wait_for_yield, TestEvaluateNamesTheHoldersYieldPolicyKeepsADraftFrom).
func TestEvaluateSaysADraftWaitsForTheHolderToYield(t *testing.T) {
	f := newYieldEvaluate(t)
	model, evaluate := f.model, f.evaluate
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
	if got := planCode(evaluate(fmt.Sprintf(manual, model.ID, ""))); got != "does_not_wait_for_yield" {
		t.Fatalf("manual plan of a never draft = %q, want does_not_wait_for_yield", got)
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

	// The answer also says whether the draft may hold a policy other than
	// never - the create's own check - so the form offers one only then.
	for _, tc := range []struct{ name, body, want string }{
		{"sleepable single-node", fmt.Sprintf(manual, model.ID, ""), `{"ok":true}`},
		{"not sleepable", fmt.Sprintf(`{"model_id":%d,"engine":"vllm","node_uuid":"yield-node"}`, model.ID), `{"ok":false,"reason":"SLEEP_NOT_ENABLED"}`},
		{"multi-node", fmt.Sprintf(`{"model_id":%d,"engine":"vllm","node_uuids":["yield-node","yield-node"],"nnodes":2,"sleepable":true}`, model.ID), `{"ok":false,"reason":"YIELD_POLICY_SHAPE"}`},
	} {
		if got := jsonOf(t, evaluate(tc.body)["yield_policy_eligible"]); got != tc.want {
			t.Errorf("%s: yield_policy_eligible = %s, want %s", tc.name, got, tc.want)
		}
	}
}

// rawPart is the literal wire JSON at path in an evaluate answer: an object
// key, or "node=<uuid>" for that node's entry of a candidates list.
func rawPart(t *testing.T, body []byte, path ...string) string {
	t.Helper()
	for _, key := range path {
		if node, ok := strings.CutPrefix(key, "node="); ok {
			var list []map[string]json.RawMessage
			if err := json.Unmarshal(body, &list); err != nil {
				t.Fatal(err)
			}
			body = nil
			for _, entry := range list {
				if string(entry["node_uuid"]) == `"`+node+`"` {
					body, _ = json.Marshal(entry)
				}
			}
			continue
		}
		var object map[string]json.RawMessage
		if err := json.Unmarshal(body, &object); err != nil {
			t.Fatal(err)
		}
		body = object[key]
	}
	return string(body)
}

// When yield policy keeps a draft off a card an awake deployment holds, the
// plan and the automatic candidate name the holder and its policy: a draft
// that never yields does not wait for a holder that would yield
// (does_not_wait_for_yield), and a holder that never yields does not give
// way to a draft that shares (held_by_non_yielding).
func TestEvaluateNamesTheHoldersYieldPolicyKeepsADraftFrom(t *testing.T) {
	f := newYieldEvaluate(t)
	manual := fmt.Sprintf(`{"model_id":%d,"engine":"vllm","node_uuid":"yield-node","sleepable":true%%s}`, f.model.ID)
	auto := fmt.Sprintf(`{"model_id":%d,"engine":"vllm","placement_mode":"auto","target_vendor":"nvidia","sleepable":true%%s}`, f.model.ID)
	check := func(policy, want string) {
		t.Helper()
		if got := rawPart(t, f.raw(fmt.Sprintf(manual, policy)), "plan", "message"); got != want {
			t.Errorf("manual plan %q = %s, want %s", policy, got, want)
		}
		if got := rawPart(t, f.raw(fmt.Sprintf(auto, policy)), "candidates", "node=yield-node", "reason"); got != want {
			t.Errorf("automatic candidate %q = %s, want %s", policy, got, want)
		}
	}
	check("", `{"code":"does_not_wait_for_yield","params":{"blockers":[],"gpus":[0],"holders":[{"deployment":"batch","policy":"anytime"}],"node":"yield-node"}}`)

	if err := f.st.SetDeploymentYieldPolicy(f.batch.ID, yieldpolicy.Policy{Yield: yieldpolicy.Never}); err != nil {
		t.Fatal(err)
	}
	check(`,"yield_policy":{"policy":"idle"}`, `{"code":"held_by_non_yielding","params":{"blockers":[{"deployment":"batch","policy":"never"}],"gpus":[0],"holders":[],"node":"yield-node"}}`)
	// A never draft beside a never holder is the card in use: yield policy
	// is not what keeps it off.
	if got := rawPart(t, f.raw(fmt.Sprintf(manual, "")), "plan", "message"); strings.Contains(got, "yield") {
		t.Errorf("never draft beside a never holder = %s", got)
	}
}
