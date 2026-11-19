package api

import (
	"errors"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestInferenceTemplateDeleteWholeTemplate(t *testing.T) {
	st, err := store.Open("file:flat_api_delete_template?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	const builtin, custom = "acme/builtin", "acme/custom"
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{flatAPIFixture(builtin, "upstream")}); err != nil {
		t.Fatal(err)
	}
	batch := flatAPIFixture(custom, "first")
	batch.Specs = append(batch.Specs, flatAPIFixture(custom, "second").Specs[0])
	if err := st.ImportUserInferenceCatalog([]store.InferenceModel{batch, flatAPIFixture(builtin, "extension")}, true); err != nil {
		t.Fatal(err)
	}
	h := &apiHandlers{d: Deps{Store: st}}
	deleteTemplate := func(id string, want int) string {
		t.Helper()
		rec := httptest.NewRecorder()
		h.deleteInferenceTemplate(rec, httptest.NewRequest(http.MethodDelete, "/api/v1/inference-templates?model_id="+url.QueryEscape(id), nil))
		if rec.Code != want {
			t.Fatalf("delete %q: status=%d want=%d body=%s", id, rec.Code, want, rec.Body.String())
		}
		return rec.Body.String()
	}
	checkBothSpecs := func(id string) *store.InferenceModel {
		t.Helper()
		m, err := st.InferenceModelByModelID(id)
		if err != nil || len(m.Specs) != 2 {
			t.Fatalf("expected both specs of %s: %+v, %v", id, m, err)
		}
		return m
	}
	deleteTemplate(" ", http.StatusBadRequest)
	deleteTemplate(builtin, http.StatusNotFound)
	checkBothSpecs(builtin)

	// A legacy/custom parent with an upstream spec must survive whole-template deletion.
	mixed := checkBothSpecs(builtin)
	if err := st.DB().Model(mixed).Update("source", "user").Error; err != nil {
		t.Fatal(err)
	}
	body := deleteTemplate(builtin, http.StatusConflict)
	if !strings.Contains(body, `"code":"INFERENCE_TEMPLATE_MIXED_SOURCES"`) || !strings.Contains(body, `"model":"acme/builtin"`) {
		t.Fatalf("missing mixed-source explanation: %s", body)
	}
	checkBothSpecs(builtin)

	model := checkBothSpecs(custom)
	pin, err := st.CreateDeployment(&store.Deployment{Name: "active-pin", TemplateID: model.ID, TemplateSpecSource: "user", TemplateSpecName: "second", State: store.DeployActive})
	if err != nil {
		t.Fatal(err)
	}
	body = deleteTemplate(custom, http.StatusConflict)
	if !strings.Contains(body, `"code":"INFERENCE_TEMPLATE_DEPLOYMENT_REFERENCE"`) || !strings.Contains(body, `"deployments":"active-pin"`) {
		t.Fatalf("missing deployment reference: %s", body)
	}
	checkBothSpecs(custom)
	if err := st.SetDeploymentState(pin.ID, store.DeployDeleting); err != nil {
		t.Fatal(err)
	}

	owner := flatAPIFixture("acme/combo", "combo")
	owner.Specs[0].Components = `[{"name":"rerank","role":"component","model_id":"acme/custom","mount_path":"/models/rerank"}]`
	if err := st.ImportUserInferenceCatalog([]store.InferenceModel{owner}, false); err != nil {
		t.Fatal(err)
	}
	body = deleteTemplate(custom, http.StatusConflict)
	if !strings.Contains(body, `"code":"INFERENCE_TEMPLATE_COMPONENT_REFERENCE"`) || !strings.Contains(body, `"owner":"acme/combo"`) || !strings.Contains(body, `"spec":"combo"`) {
		t.Fatalf("missing component reference: %s", body)
	}
	checkBothSpecs(custom)
	deleteTemplate(owner.ModelID, http.StatusOK)
	deleteTemplate(custom, http.StatusOK)
	deleteTemplate(custom, http.StatusNotFound)
	checkBothSpecs(builtin)
}

func TestInferenceTemplateDeleteUserSpecScenarios(t *testing.T) {
	st, err := store.Open("file:flat_api_delete_spec?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{
		flatAPIFixture("acme/builtin", "general"),
	}); err != nil {
		t.Fatal(err)
	}
	userSpec := func(name string) store.InferenceSpecRow {
		return store.InferenceSpecRow{
			Name: name, Engine: "sglang", Image: "custom/image:latest", Quantization: "fp8",
			WeightSources: `[{"provider":"huggingface","repo_id":"acme/useronly"}]`, GPUs: `{"vendor":"nvidia"}`,
		}
	}
	if err := st.ImportUserInferenceCatalog([]store.InferenceModel{{
		ModelID: "acme/useronly", Source: "user", Specs: []store.InferenceSpecRow{userSpec("solo"), userSpec("extra")},
	}}, true); err != nil {
		t.Fatal(err)
	}
	h := &apiHandlers{d: Deps{Store: st}}
	call := func(modelID, spec string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodDelete, "/api/v1/inference-templates/spec?model_id="+url.QueryEscape(modelID)+"&spec="+url.QueryEscape(spec), nil)
		rec := httptest.NewRecorder()
		h.deleteInferenceTemplateSpec(rec, req)
		return rec
	}
	// validation: missing params
	if rec := call("", ""); rec.Code != http.StatusBadRequest {
		t.Fatalf("missing params status=%d body=%s", rec.Code, rec.Body.String())
	}
	// unknown model and unknown spec both 404
	if rec := call("acme/missing", "general"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown model status=%d body=%s", rec.Code, rec.Body.String())
	}
	if rec := call("acme/builtin", "nope"); rec.Code != http.StatusNotFound {
		t.Fatalf("unknown spec status=%d body=%s", rec.Code, rec.Body.String())
	}
	// upstream specs are not deletable through the user-spec path
	if rec := call("acme/builtin", "general"); rec.Code != http.StatusNotFound {
		t.Fatalf("upstream spec status=%d body=%s", rec.Code, rec.Body.String())
	}
	// Historical deployments can pin only the parent template. They allow
	// removing an individual spec while another spec keeps the parent alive.
	parent, err := st.InferenceModelByModelID("acme/useronly")
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := st.CreateDeployment(&store.Deployment{Name: "legacy-pin", TemplateID: parent.ID, State: store.DeployActive})
	if err != nil {
		t.Fatal(err)
	}
	if rec := call("acme/useronly", "extra"); rec.Code != http.StatusOK {
		t.Fatalf("delete user spec status=%d body=%s", rec.Code, rec.Body.String())
	}
	model, err := st.InferenceModelByModelID("acme/useronly")
	if err != nil || len(model.Specs) != 1 || model.Specs[0].Name != "solo" {
		t.Fatalf("after delete model=%+v err=%v", model, err)
	}
	// Removing the final spec would remove that pinned template as well.
	rec := call("acme/useronly", "solo")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"code":"INFERENCE_TEMPLATE_DEPLOYMENT_REFERENCE"`) || !strings.Contains(rec.Body.String(), `"deployments":"legacy-pin"`) {
		t.Fatalf("last spec with legacy pin: status=%d body=%s", rec.Code, rec.Body.String())
	}
	if survivor, err := st.InferenceModelByModelID("acme/useronly"); err != nil || len(survivor.Specs) != 1 || survivor.Specs[0].ID != model.Specs[0].ID {
		t.Fatalf("blocked delete must preserve the parent and final spec: %+v, %v", survivor, err)
	}
	if err := st.SetDeploymentState(legacy.ID, store.DeployDeleting); err != nil {
		t.Fatal(err)
	}
	// deleting the last user spec drops the whole user model
	if rec := call("acme/useronly", "solo"); rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"model_deleted":true`) {
		t.Fatalf("last spec delete status=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := st.InferenceModelByModelID("acme/useronly"); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("model row survived last spec delete: err=%v", err)
	}
}

func TestInferenceTemplateDeleteUserSpecRefusesWhileReferenced(t *testing.T) {
	st, err := store.Open("file:flat_api_delete_ref?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{flatAPIFixture("acme/base", "general")}); err != nil {
		t.Fatal(err)
	}
	userSpecs := func(name string) []store.InferenceSpecRow {
		return []store.InferenceSpecRow{{
			Name: name, Engine: "sglang", Image: "custom/image:latest", Quantization: "bf16",
			WeightSources: `[{"provider":"huggingface","repo_id":"acme/base"}]`, GPUs: `{"vendor":"nvidia"}`,
		}}
	}
	if err := st.ImportUserInferenceCatalog([]store.InferenceModel{{ModelID: "acme/base", Source: "user", Specs: userSpecs("tuned")}}, true); err != nil {
		t.Fatal(err)
	}
	base, err := st.InferenceModelByModelID("acme/base")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{{
		ModelID: "acme/combo", Source: "upstream", Specs: []store.InferenceSpecRow{{
			Name: "combo", Engine: "vllm", Image: "vllm/vllm-openai:latest", Quantization: "bf16",
			WeightSources: `[{"provider":"huggingface","repo_id":"acme/combo"}]`, GPUs: `{"vendor":"nvidia"}`,
			Components: `[{"name":"rerank","role":"component","model_id":"acme/useronly","mount_path":"/models/rerank"}]`,
		}},
	}}); err != nil {
		t.Fatal(err)
	}
	if err := st.ImportUserInferenceCatalog([]store.InferenceModel{{ModelID: "acme/useronly", Source: "user", Specs: userSpecs("only")}}, true); err != nil {
		t.Fatal(err)
	}
	h := &apiHandlers{d: Deps{Store: st}}
	// a deployment pinning the spec blocks deletion with 409
	pinner, err := st.CreateDeployment(&store.Deployment{Name: "pinned", TemplateID: base.ID, TemplateSpecSource: "user", TemplateSpecName: "tuned", State: store.DeployActive})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodDelete, "/api/v1/inference-templates/spec?model_id=acme%2Fbase&spec=tuned", nil)
	rec := httptest.NewRecorder()
	h.deleteInferenceTemplateSpec(rec, req)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"code":"INFERENCE_SPEC_DEPLOYMENT_REFERENCE"`) || !strings.Contains(rec.Body.String(), `"spec":"tuned"`) || !strings.Contains(rec.Body.String(), `"deployments":"pinned"`) {
		t.Fatalf("in-use status=%d body=%s", rec.Code, rec.Body.String())
	}
	// a deleting deployment no longer pins
	if err := st.SetDeploymentState(pinner.ID, store.DeployDeleting); err != nil {
		t.Fatal(err)
	}
	rec = httptest.NewRecorder()
	h.deleteInferenceTemplateSpec(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("after deleting deployment status=%d body=%s", rec.Code, rec.Body.String())
	}
	// dropping the last spec of a model referenced as a component is refused
	req = httptest.NewRequest(http.MethodDelete, "/api/v1/inference-templates/spec?model_id=acme%2Fuseronly&spec=only", nil)
	rec = httptest.NewRecorder()
	h.deleteInferenceTemplateSpec(rec, req)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "acme/combo") {
		t.Fatalf("component reference status=%d body=%s", rec.Code, rec.Body.String())
	}
	if _, err := st.InferenceModelByModelID("acme/useronly"); err != nil {
		t.Fatalf("referenced model must survive: %v", err)
	}
}
