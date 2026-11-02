package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestEvaluateAcquisitionDeclaredSourcesAndCacheWire(t *testing.T) {
	st := newAcquisitionStore(t)
	fixture := deploymentFlatFixture()
	fixture.Specs[0].GPUs = `{}`
	fixture.Specs[0].WeightSources = `[{"provider":"huggingface","repo_id":"Acme/Deploy-FP8","revision":"hf-pin"},{"provider":"modelscope","repo_id":"Acme/Deploy-FP8","revision":"ms-pin"}]`
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{fixture}); err != nil {
		t.Fatal(err)
	}
	template, err := st.InferenceModelByModelID(fixture.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("acq", store.HashToken("token"), "acq")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.SetSetting("network", `{"rewrites":[{"from":"https://huggingface.co","to":"https://hf-mirror.com"}],"http_proxy":"http://proxy.example:7890"}`); err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name, provider, reason string
		cached                 *bool
		online                 bool
		failure                error
	}{
		{"present", "huggingface", "", boolPointer(true), true, nil},
		{"absent", "modelscope", "", boolPointer(false), true, nil},
		{"failed", "huggingface", "query_failed", nil, true, errors.New("runtime unavailable")},
		{"offline", "huggingface", "node_offline", nil, false, nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			calls := 0
			h := &apiHandlers{d: Deps{Store: st, Online: func(string) bool { return tc.online }, ImageExists: func(_ context.Context, ref, uuid string) (bool, error) {
				calls++
				if ref != fixture.Specs[0].Image || uuid != node.UUID {
					t.Fatalf("query %s %s", ref, uuid)
				}
				return tc.cached != nil && *tc.cached, tc.failure
			}}}
			body := fmt.Sprintf(`{"template_id":%d,"template_spec_source":"upstream","template_spec_name":"fp8-balanced","node_uuid":"acq","model_source":{"provider":%q},"quick_params":{},"placement":{}}`, template.ID, tc.provider)
			rec := httptest.NewRecorder()
			h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
			if rec.Code != http.StatusOK {
				t.Fatalf("%d %s", rec.Code, rec.Body.String())
			}
			var wire map[string]json.RawMessage
			if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil {
				t.Fatal(err)
			}
			var acq struct {
				Providers []map[string]any `json:"providers"`
				Image     map[string]any   `json:"image"`
				Nodes     []map[string]any `json:"nodes"`
			}
			if err := json.Unmarshal(wire["acquisition"], &acq); err != nil {
				t.Fatal(err)
			}
			if len(acq.Providers) != 2 || acq.Providers[0]["label"] != "huggingface" || acq.Providers[1]["label"] != "modelscope" || acq.Providers[0]["recommended"] != true {
				t.Fatalf("providers=%+v", acq.Providers)
			}
			v, exists := acq.Image["cached"]
			if exists != (tc.cached != nil) || (exists && v != *tc.cached) {
				t.Fatalf("cache=%+v", acq.Image)
			}
			if tc.reason != "" && acq.Image["cache_reason"] != tc.reason {
				t.Fatalf("reason=%+v", acq.Image)
			}
			if !tc.online && calls != 0 {
				t.Fatal("offline node queried")
			}
			// The node's own forecast carries the same cache fact, and the
			// network routes of the earlier form are gone from the wire.
			if _, ok := wireObject(t, wire["acquisition"])["networks"]; ok {
				t.Fatalf("networks are still on the wire: %s", wire["acquisition"])
			}
			if len(acq.Nodes) != 1 || acq.Nodes[0]["node_uuid"] != node.UUID {
				t.Fatalf("nodes=%+v", acq.Nodes)
			}
			nodeCached, nodeHas := acq.Nodes[0]["image_cached"]
			if nodeHas != exists || (nodeHas && nodeCached != v) || acq.Nodes[0]["image_cache_reason"] != acq.Image["cache_reason"] {
				t.Fatalf("node forecast %+v disagrees with image %+v", acq.Nodes[0], acq.Image)
			}
			wantPlan := "pull"
			if tc.cached != nil && *tc.cached {
				wantPlan = "present"
			}
			if acq.Nodes[0]["image_plan"] != wantPlan {
				t.Fatalf("image_plan=%v, want %s", acq.Nodes[0]["image_plan"], wantPlan)
			}
		})
	}
	h := &apiHandlers{d: Deps{Store: st}}
	body := fmt.Sprintf(`{"template_id":%d,"template_spec_source":"upstream","template_spec_name":"fp8-balanced","model_source":{"provider":"unknown"}}`, template.ID)
	rec := httptest.NewRecorder()
	h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
	if rec.Code != 400 || !strings.Contains(rec.Body.String(), ErrorAcquisitionCandidateStale) {
		t.Fatalf("stale=%d %s", rec.Code, rec.Body.String())
	}
}

func boolPointer(v bool) *bool { return &v }

// wireObject decodes a raw JSON object so a test can ask which keys are on
// the wire.
func wireObject(t *testing.T, raw json.RawMessage) map[string]json.RawMessage {
	t.Helper()
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		t.Fatalf("%s: %v", raw, err)
	}
	return object
}

// Regression: a model registered from ModelScope without a revision, already
// downloaded on the target node, evaluated against a catalog spec that pins a
// full commit for both providers. Before the repository-id match this returned
// "model_id conflicts with template model", which left the response without an
// acquisition block at all, so the deploy form waited forever on "checking what
// this node already has".
func TestEvaluateReusesUnpinnedNodeCopyForAPinnedTemplateSource(t *testing.T) {
	st := newAcquisitionStore(t)
	fixture := deploymentFlatFixture()
	fixture.ModelID = "qwen/qwen3.5-0.8b"
	fixture.Specs[0].WeightSources = `[{"provider":"modelscope","repo_id":"Qwen/Qwen3.5-0.8B","revision":"4d58a7b524cd0a41f80a7fbb6a5b22f2fca1e3c9"},` +
		`{"provider":"huggingface","repo_id":"Qwen/Qwen3.5-0.8B","revision":"2fc0636424b8a1e0c1d1f0d3d1c0a9b8f7e6d5c4"}]`
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{fixture}); err != nil {
		t.Fatal(err)
	}
	template, err := st.InferenceModelByModelID(fixture.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("mirror-node", store.HashToken("token"), "mirror-node")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.DB().Create(&store.GPU{
		NodeID: node.ID, Index: 0, UUID: "GPU-mirror-0", Name: "NVIDIA H100", Vendor: "nvidia",
		ComputeCapability: "9.0", ArchID: "sm_90", MemoryTotal: 80 << 30,
	}).Error; err != nil {
		t.Fatal(err)
	}
	registered, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "Qwen/Qwen3.5-0.8B", Revision: "", Name: "Qwen3.5-0.8B"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: registered.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/qwen3.5-0.8b", SizeBytes: 1 << 30}); err != nil {
		t.Fatal(err)
	}

	h := &apiHandlers{d: Deps{Store: st, Online: func(string) bool { return true }}}
	body := fmt.Sprintf(`{"template_id":%d,"template_spec_source":"upstream","template_spec_name":"fp8-balanced","node_uuid":%q,"model_id":%d,"quick_params":{},"placement":{}}`,
		template.ID, node.UUID, registered.ID)
	rec := httptest.NewRecorder()
	h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		OK          bool `json:"ok"`
		Acquisition *struct {
			Present []struct {
				Key       string `json:"key"`
				StorageID uint   `json:"storage_id"`
				OnNode    bool   `json:"on_node"`
			} `json:"present"`
			Providers []map[string]any `json:"providers"`
			Nodes     []map[string]any `json:"nodes"`
		} `json:"acquisition"`
		Warnings []map[string]any `json:"warnings"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	if !resp.OK {
		t.Fatalf("ok=false warnings=%+v body=%s", resp.Warnings, rec.Body.String())
	}
	if resp.Acquisition == nil {
		t.Fatalf("response carried no acquisition, the form would wait forever: %s", rec.Body.String())
	}
	if len(resp.Acquisition.Present) != 1 || !resp.Acquisition.Present[0].OnNode {
		t.Fatalf("the copy already on the node was not offered as present: %s", rec.Body.String())
	}
	if len(resp.Acquisition.Nodes) != 1 || resp.Acquisition.Nodes[0]["weights_plan"] != "present" || resp.Acquisition.Nodes[0]["weights_present"] != true {
		t.Fatalf("the node forecast does not use the copy on the node: %+v", resp.Acquisition.Nodes)
	}
	if len(resp.Acquisition.Providers) != 2 {
		t.Fatalf("declared providers=%+v", resp.Acquisition.Providers)
	}
	models, err := st.ListModels()
	if err != nil || len(models) != 1 || models[0].ID != registered.ID {
		t.Fatalf("models=%+v err=%v, want no duplicate registration", models, err)
	}
}

func TestEmptyRevisionResolvesPinnedLocalIdentity(t *testing.T) {
	st := newAcquisitionStore(t)
	node, err := st.RegisterNode("pinned", store.HashToken("token"), "pinned")
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "Acme/Model", Revision: "commit"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/pinned"}); err != nil {
		t.Fatal(err)
	}
	h := &apiHandlers{d: Deps{Store: st}}
	source := weightSource{Provider: "huggingface", RepoID: "Acme/Model"}
	if !templateWeightMatchesModel(source, model) {
		t.Fatal("a registered copy of the same repository must satisfy an unpinned source")
	}
	selected, err := h.selectWeightSource(store.InferenceSpecRow{WeightSources: `[{"provider":"huggingface","repo_id":"Acme/Model"}]`}, node.ID, nil)
	if err != nil || selected.Revision != "commit" {
		t.Fatalf("selection=%+v %v", selected, err)
	}
	got, err := h.resolveTemplateWeightModelForNode(source, &store.InferenceModel{ModelID: "acme/model"}, "vllm", "", 0, node.ID, nil)
	if err != nil || got.ID != model.ID {
		t.Fatalf("model=%+v %v", got, err)
	}
	models, err := st.ListModels()
	if err != nil || len(models) != 1 {
		t.Fatalf("models=%v %v", models, err)
	}
}

func TestExplicitProviderConstrainsPublicDownloadIdentity(t *testing.T) {
	st := newAcquisitionStore(t)
	hf, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "Acme/Model", Revision: "hf"})
	if err != nil {
		t.Fatal(err)
	}
	h := &apiHandlers{d: Deps{Store: st}}
	selection := templateSelection{Model: &store.InferenceModel{ModelID: "acme/model"}, Spec: &store.InferenceSpecRow{Engine: "vllm", WeightSources: `[{"provider":"huggingface","repo_id":"Acme/Model","revision":"hf"},{"provider":"modelscope","repo_id":"Acme/Model","revision":"ms"}]`}}
	if err := h.selectExplicitWeight(&selection, &dto.ModelSource{Provider: "modelscope"}); err != nil {
		t.Fatal(err)
	}
	if _, err := h.resolveDeploymentModelFromTemplate(context.Background(), hf.ID, &selection, 1); err == nil {
		t.Fatal("conflicting model_id and provider accepted")
	}
	model, err := h.resolveDeploymentModelFromTemplate(context.Background(), 0, &selection, 1)
	if err != nil || model.Source != "modelscope" || model.Revision != "ms" {
		t.Fatalf("model=%+v %v", model, err)
	}
	unrelated, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "Other/Model", Revision: "hf"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := h.resolveDeploymentModelFromTemplate(context.Background(), unrelated.ID, &selection, 1); err == nil {
		t.Fatal("unrelated model_id accepted")
	}
}

// A request that chooses a provider moves the draft along with the primary:
// a node that reaches only ModelScope would otherwise download the primary
// from it and then stall on a draft still bound to Hugging Face. A component
// that declares nothing at the chosen provider keeps the source it had.
func TestExplicitProviderAlsoSelectsComponentSources(t *testing.T) {
	st := newAcquisitionStore(t)
	h := &apiHandlers{d: Deps{Store: st}}
	spec := store.InferenceSpecRow{Name: "glm-tp2", Engine: "callable",
		WeightSources: `[{"provider":"huggingface","repo_id":"Acme/Main","revision":"hf-main"},{"provider":"modelscope","repo_id":"Acme/Main","revision":"ms-main"}]`,
		Components: `[
		{"name":"primary","role":"primary","weights":{"quantization":"q4","sources":[{"provider":"huggingface","repo_id":"Acme/Main","revision":"hf-main"},{"provider":"modelscope","repo_id":"Acme/Main","revision":"ms-main"}]},"weight_bytes":100,"mount_path":"/model"},
		{"name":"draft","role":"speculative_draft","model_id":"acme/draft","weights":{"quantization":"bf16","sources":[{"provider":"huggingface","repo_id":"Acme/Draft","revision":"hf-draft"},{"provider":"modelscope","repo_id":"Mirror/Draft","revision":"ms-draft"}]},"weight_bytes":10,"mount_path":"/draft"},
		{"name":"verifier","role":"verifier","model_id":"acme/verifier","weights":{"quantization":"bf16","sources":[{"provider":"huggingface","repo_id":"Acme/Verifier","revision":"hf-verifier"}]},"weight_bytes":10,"mount_path":"/verifier"}]`}
	selection := templateSelection{Model: &store.InferenceModel{ModelID: "acme/main"}, Spec: &spec}
	if err := h.selectTemplateComponents(&selection, spec, 0); err != nil {
		t.Fatal(err)
	}
	if err := h.selectExplicitWeight(&selection, &dto.ModelSource{Provider: "modelscope"}); err != nil {
		t.Fatal(err)
	}
	want := map[string][3]string{
		"primary":  {"modelscope", "Acme/Main", "ms-main"},
		"draft":    {"modelscope", "Mirror/Draft", "ms-draft"},
		"verifier": {"huggingface", "Acme/Verifier", "hf-verifier"},
	}
	for _, c := range selection.Components {
		if got := [3]string{c.Provider, c.RepoID, c.Revision}; got != want[c.Name] {
			t.Fatalf("component %s source = %v, want %v", c.Name, got, want[c.Name])
		}
	}
	if len(selection.Components) != len(want) {
		t.Fatalf("components = %+v", selection.Components)
	}
}

// A draft published under the same repository id at both providers resolves to
// the provider the request chose even when a mirror row of that repository is
// already registered. Matching by repository alone would hand back the
// Hugging Face row, so the draft would still be fetched from the provider the
// node cannot reach.
func TestExplicitProviderComponentIgnoresARegisteredMirrorRow(t *testing.T) {
	st := newAcquisitionStore(t)
	hfDraft, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "Acme/Draft", Revision: "hf-draft"})
	if err != nil {
		t.Fatal(err)
	}
	primary, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "Acme/Main", Revision: "ms-main"})
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"acme/main", "acme/draft"} {
		fixture := deploymentFlatFixture()
		fixture.ModelID = id
		if err := st.ReconcileInferenceCatalog([]store.InferenceModel{fixture}); err != nil {
			t.Fatal(err)
		}
	}
	h := &apiHandlers{d: Deps{Store: st}}
	spec := store.InferenceSpecRow{Name: "glm-tp2", Engine: "callable",
		WeightSources: `[{"provider":"huggingface","repo_id":"Acme/Main","revision":"hf-main"},{"provider":"modelscope","repo_id":"Acme/Main","revision":"ms-main"}]`,
		Components: `[
		{"name":"primary","role":"primary","weights":{"quantization":"q4","sources":[{"provider":"huggingface","repo_id":"Acme/Main","revision":"hf-main"},{"provider":"modelscope","repo_id":"Acme/Main","revision":"ms-main"}]},"weight_bytes":100,"mount_path":"/model"},
		{"name":"draft","role":"speculative_draft","model_id":"acme/draft","weights":{"quantization":"bf16","sources":[{"provider":"huggingface","repo_id":"Acme/Draft","revision":"hf-draft"},{"provider":"modelscope","repo_id":"Acme/Draft","revision":"ms-draft"}]},"weight_bytes":10,"mount_path":"/draft"}]`}
	selection := templateSelection{Model: &store.InferenceModel{ModelID: "acme/main"}, Spec: &spec}
	if err := h.selectTemplateComponents(&selection, spec, 0); err != nil {
		t.Fatal(err)
	}
	if err := h.selectExplicitWeight(&selection, &dto.ModelSource{Provider: "modelscope"}); err != nil {
		t.Fatal(err)
	}
	if err := h.resolveTemplateComponentModels(&selection, primary); err != nil {
		t.Fatal(err)
	}
	for _, c := range selection.Components {
		model, err := st.GetModel(c.StoreModelID)
		if err != nil {
			t.Fatalf("component %s: %v", c.Name, err)
		}
		if model.Source != "modelscope" {
			t.Fatalf("component %s bound to %s row %d, want modelscope", c.Name, model.Source, model.ID)
		}
		if c.Name == "draft" && (model.ID == hfDraft.ID || model.Revision != "ms-draft") {
			t.Fatalf("draft model = %+v, want a new modelscope row at ms-draft", model)
		}
	}
}
