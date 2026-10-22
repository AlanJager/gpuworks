package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/chipmatch"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

const (
	imageSourcesDigest   = "sha256:22789f0c1a2b3c4d5e6f708192a3b4c5d6e7f8091a2b3c4d5e6f708192a3b4c5"
	imageSourcesUpstream = "ghcr.io/org/runner@" + imageSourcesDigest
	imageSourcesSWR      = "swr.cn-southwest-2.myhuaweicloud.com/gpuworks/runner@" + imageSourcesDigest
	imageSourcesNJU      = "ghcr.nju.edu.cn/org/runner:v0.6.0"
)

// imageSourcesFixture is a template whose digest-pinned image names two further
// sources: a byte-identical copy and a mirror that is only a tag reference.
func imageSourcesFixture() store.InferenceModel {
	return store.InferenceModel{
		ModelID: "acme/sources", DisplayTitle: "Sources", Source: "upstream", Architecture: "qwen3", ContextLength: 8192,
		Specs: []store.InferenceSpecRow{{
			Name: "fp8-balanced", Scenario: "balanced", Engine: "vllm", Image: imageSourcesUpstream,
			ImageSources: fmt.Sprintf(`[%q,%q]`, imageSourcesSWR, imageSourcesNJU),
			Quantization: "fp8", WeightBytes: 4 << 30,
			WeightSources: `[{"provider":"huggingface","repo_id":"Acme/Sources-FP8"}]`,
			GPUs:          `{"vendor":"nvidia","cc":">=8.9"}`, Args: `["--max-model-len","4096"]`,
		}},
	}
}

type imageSourcesRig struct {
	st       *store.Store
	h        *apiHandlers
	template *store.InferenceModel
	model    *store.Model
	nodeUUID string
}

func newImageSourcesRig(t *testing.T, name string) imageSourcesRig {
	t.Helper()
	st, err := store.Open("file:api_image_sources_"+name+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	fixture := imageSourcesFixture()
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{fixture}); err != nil {
		t.Fatal(err)
	}
	template, err := st.InferenceModelByModelID(fixture.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "Acme/Sources-FP8", Revision: "main", Name: "Acme/Sources-FP8", WeightBytes: 4 << 30, QuantLabel: "fp8",
		Meta: `{"model_type":"llama","max_position_embeddings":8192,"num_hidden_layers":2,"hidden_size":64,"num_attention_heads":4,"num_key_value_heads":4,"torch_dtype":"bfloat16"}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	launchConfigNode(t, st, "image-sources-node")
	return imageSourcesRig{st: st, h: &apiHandlers{d: Deps{Store: st}}, template: template, model: model, nodeUUID: "image-sources-node"}
}

func (r imageSourcesRig) body(extra string) string {
	return fmt.Sprintf(`{"name":"sources","template_id":%d,"model_id":%d,"node_uuid":%q,"quick_params":{}%s}`, r.template.ID, r.model.ID, r.nodeUUID, extra)
}

func (r imageSourcesRig) evaluate(t *testing.T, extra string) dto.EvaluateResponse {
	t.Helper()
	rec := httptest.NewRecorder()
	r.h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(r.body(extra))))
	if rec.Code != http.StatusOK {
		t.Fatalf("evaluate = %d: %s", rec.Code, rec.Body.String())
	}
	var out dto.EvaluateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
		t.Fatal(err)
	}
	return out
}

func (r imageSourcesRig) create(t *testing.T, extra string) (int, string) {
	t.Helper()
	rec := httptest.NewRecorder()
	r.h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(r.body(extra))))
	return rec.Code, rec.Body.String()
}

func (r imageSourcesRig) storedSelection(t *testing.T, createdBody string) (*store.Deployment, deploy.CatalogSelection) {
	t.Helper()
	var created dto.Deployment
	if err := json.Unmarshal([]byte(createdBody), &created); err != nil {
		t.Fatal(err)
	}
	dep, err := r.st.GetDeployment(created.ID)
	if err != nil {
		t.Fatal(err)
	}
	selection, err := deploy.DecodeCatalogSelection([]byte(dep.CatalogSelectionSnapshot))
	if err != nil {
		t.Fatal(err)
	}
	return dep, selection
}

// jsonEqual compares two JSON documents by value, so a test can state the wire
// shape literally without depending on key order.
func jsonEqual(t *testing.T, got any, want string) {
	t.Helper()
	encoded, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	var gotValue, wantValue any
	if err := json.Unmarshal(encoded, &gotValue); err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal([]byte(want), &wantValue); err != nil {
		t.Fatalf("expected document is not JSON: %v", err)
	}
	if !reflect.DeepEqual(gotValue, wantValue) {
		t.Fatalf("wire JSON = %s\nwant      %s", encoded, want)
	}
}

// The evaluation forecasts a manually placed node's pull: the template's
// sources first, in the order it declared them, then the image itself. With no
// pull source rules every copy is ranked by how fast the node reaches it.
func TestEvaluateForecastsTemplateImageSourcesAsPullRefs(t *testing.T) {
	rig := newImageSourcesRig(t, "evaluate")
	rig.h.d.Online = func(string) bool { return true }
	rig.h.d.ImageExists = func(context.Context, string, string) (bool, error) { return false, nil }
	out := rig.evaluate(t, "")
	if out.Acquisition == nil || out.Acquisition.Image.Image != imageSourcesUpstream {
		t.Fatalf("acquisition image = %+v", out.Acquisition)
	}
	jsonEqual(t, out.Acquisition.Nodes, fmt.Sprintf(`[
		{"node_uuid":"image-sources-node","node_name":"image-sources-node","image_cached":false,"image_plan":"pull",
		 "image_pull_refs":[%q,%q,%q],"image_pull_ranked":true,
		 "weights_present":false,"weights_plan":"download","weights_provider":"huggingface","weights_fetch_hosts":["huggingface.co"]}
	]`, imageSourcesSWR, imageSourcesNJU, imageSourcesUpstream))
	if out.TemplateDeviated {
		t.Fatal("offering sources is not a deviation")
	}
}

// A request that overrides the image runs something the template did not
// describe, so none of the template's sources are in its pull forecast.
func TestEvaluateWithAnImageOverrideForecastsNoTemplateSources(t *testing.T) {
	rig := newImageSourcesRig(t, "evaluate_override")
	out := rig.evaluate(t, `,"image":"custom/vllm:cuda"`)
	if out.Acquisition == nil || len(out.Acquisition.Nodes) != 1 {
		t.Fatalf("acquisition = %+v", out.Acquisition)
	}
	node := out.Acquisition.Nodes[0]
	if node.ImagePlan != "pull" || !reflect.DeepEqual(node.ImagePullRefs, []string{"custom/vllm:cuda"}) {
		t.Fatalf("forecast = %+v", node)
	}
}

// The request no longer chooses an image source: a stale client that still
// sends an image_pull choice is ignored (naming a source the template declared
// and one it did not alike), the deployment is not a departure from the
// template, and the node plans its pull from the template's sources and the
// pull settings as if nothing had been sent.
func TestCreateIgnoresALegacyImagePullChoice(t *testing.T) {
	for name, provider := range map[string]string{"declared": imageSourcesNJU, "undeclared": "elsewhere.example/org/runner:v1"} {
		t.Run(name, func(t *testing.T) {
			rig := newImageSourcesRig(t, "create_legacy_"+name)
			status, body := rig.create(t, fmt.Sprintf(`,"image_pull":{"image":%q,"kind":"public","provider":%q,"network":{"mode":"direct"}}`, imageSourcesUpstream, provider))
			if status != http.StatusCreated {
				t.Fatalf("create = %d: %s", status, body)
			}
			dep, selection := rig.storedSelection(t, body)
			if dep.TemplateDeviated || len(selection.DeviatedFields) != 0 {
				t.Fatalf("a legacy choice deviated: deployment=%v fields=%v", dep.TemplateDeviated, selection.DeviatedFields)
			}
			if dep.Image != imageSourcesUpstream || selection.Image != imageSourcesUpstream {
				t.Fatalf("image = %q / %q: the sources do not replace the template's identity", dep.Image, selection.Image)
			}
			if want := []string{imageSourcesSWR, imageSourcesNJU}; !reflect.DeepEqual(selection.ImageSources, want) {
				t.Fatalf("snapshot image sources = %v, want %v", selection.ImageSources, want)
			}
			if strings.Contains(dep.CatalogSelectionSnapshot, "acquisition") || strings.Contains(dep.CatalogSelectionSnapshot, "elsewhere.example") {
				t.Fatalf("the legacy choice reached the snapshot: %s", dep.CatalogSelectionSnapshot)
			}
			node, err := rig.st.NodeByUUID(rig.nodeUUID)
			if err != nil {
				t.Fatal(err)
			}
			plan, err := planning.ImagePullPlanFor(rig.st, nil, planning.LaunchSourceFromDeployment(dep), node, selection)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(plan.Refs(), []string{imageSourcesSWR, imageSourcesNJU, imageSourcesUpstream}) {
				t.Fatalf("plan = %+v, want the full declared chain", plan)
			}
		})
	}
}

// Without a choice the node tries the template's sources in the declared order
// and the image itself last.
func TestCreateWithoutAChoiceTriesTemplateSourcesThenTheImage(t *testing.T) {
	rig := newImageSourcesRig(t, "create_default")
	status, body := rig.create(t, "")
	if status != http.StatusCreated {
		t.Fatalf("create = %d: %s", status, body)
	}
	dep, selection := rig.storedSelection(t, body)
	if dep.TemplateDeviated {
		t.Fatalf("an untouched template deploy deviated: %v", selection.DeviatedFields)
	}
	node, err := rig.st.NodeByUUID(rig.nodeUUID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planning.ImagePullPlanFor(rig.st, nil, planning.LaunchSourceFromDeployment(dep), node, selection)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(plan.Refs(), []string{imageSourcesSWR, imageSourcesNJU, imageSourcesUpstream}) {
		t.Fatalf("plan = %+v", plan)
	}
}

// With an image override the template's sources are not on offer, and the
// deployment's snapshot carries none.
func TestCreateWithAnImageOverrideDoesNotRecordTemplateSources(t *testing.T) {
	rig := newImageSourcesRig(t, "create_override")
	status, body := rig.create(t, `,"image":"custom/vllm:cuda"`)
	if status != http.StatusCreated {
		t.Fatalf("create = %d: %s", status, body)
	}
	dep, selection := rig.storedSelection(t, body)
	if !dep.TemplateDeviated || !reflect.DeepEqual(selection.DeviatedFields, []string{"image"}) {
		t.Fatalf("an image override must deviate: %v %v", dep.TemplateDeviated, selection.DeviatedFields)
	}
	if len(selection.ImageSources) != 0 || strings.Contains(dep.CatalogSelectionSnapshot, "image_sources") {
		t.Fatalf("an overridden image recorded the template's sources: %s", dep.CatalogSelectionSnapshot)
	}
	node, err := rig.st.NodeByUUID(rig.nodeUUID)
	if err != nil {
		t.Fatal(err)
	}
	plan, err := planning.ImagePullPlanFor(rig.st, nil, planning.LaunchSourceFromDeployment(dep), node, selection)
	if err != nil || !reflect.DeepEqual(plan.Refs(), []string{"custom/vllm:cuda"}) {
		t.Fatalf("plan = %+v, %v", plan, err)
	}
}

// The pull forecast of an image that is also an image-catalog row: the
// template's sources, then the row's registry sources, then the canonical ref,
// without repeats; an archive URL is not a registry reference to pull.
func TestForecastPullRefsOrderTemplateThenCatalogThenCanonical(t *testing.T) {
	st := newAcquisitionStore(t)
	node, err := st.RegisterNode("catalog-order-node", store.HashToken("token"), "catalog-order-node")
	if err != nil {
		t.Fatal(err)
	}
	registry, _ := json.Marshal([]string{"registry.example/gpustack/runner:v1", "https://files.example/runner.tar", "mirror.example/gpustack/runner:v1"})
	if _, err := st.UpsertImage(&store.Image{ImageRef: "gpustack/runner:v1", Name: "runner", Engine: "vllm", Enabled: true, RegistrySources: string(registry)}); err != nil {
		t.Fatal(err)
	}
	h := &apiHandlers{d: Deps{Store: st}}
	templateSources := []string{"team.example/gpustack/runner:v1", " mirror.example/gpustack/runner:v1 ", "team.example/gpustack/runner:v1", ""}

	facts := h.evaluateNodeAcquisition(context.Background(), []string{node.UUID}, "gpustack/runner:v1", templateSources, nil, store.SourceRules{}, nil)
	if len(facts) != 1 || facts[0].ImagePlan != "pull" {
		t.Fatalf("forecast = %+v", facts)
	}
	want := []string{"team.example/gpustack/runner:v1", "mirror.example/gpustack/runner:v1", "registry.example/gpustack/runner:v1", "gpustack/runner:v1"}
	if !reflect.DeepEqual(facts[0].ImagePullRefs, want) {
		t.Fatalf("pull refs = %v, want %v", facts[0].ImagePullRefs, want)
	}
}

// The refs the evaluation forecasts and the chain a deployment falls back to
// are one ordering: for an image the catalog does not know, both are the
// template's sources, then the image, with a reference repeated in another
// spelling (Docker Hub default, host case) tried once, where it first
// appeared.
func TestForecastPullRefsAndFallbackChainShareOneOrdering(t *testing.T) {
	st := newAcquisitionStore(t)
	node, err := st.RegisterNode("ordering-node", store.HashToken("token"), "ordering-node")
	if err != nil {
		t.Fatal(err)
	}
	const image = "org/runner:v1"
	sources := []string{"Team.Example/org/runner:v1", "team.example/org/runner:v1", "docker.io/org/runner:v1", "ghcr.io/org/runner:V1"}
	h := &apiHandlers{d: Deps{Store: st}}
	facts := h.evaluateNodeAcquisition(context.Background(), []string{node.UUID}, image, sources, nil, store.SourceRules{}, nil)
	plan, err := planning.ImagePullPlanFor(st, nil, planning.LaunchSourceFromDeployment(&store.Deployment{ID: 1, Image: image}), node,
		deploy.CatalogSelection{Image: image, ImageSources: sources})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"Team.Example/org/runner:v1", "docker.io/org/runner:v1", "ghcr.io/org/runner:V1"}
	if len(facts) != 1 || !reflect.DeepEqual(facts[0].ImagePullRefs, want) || !reflect.DeepEqual(plan.Refs(), want) {
		t.Fatalf("forecast = %+v, fallback chain = %v, want both %v", facts, plan.Refs(), want)
	}
}

// The ownership rule: only the template's own image gets its sources.
func TestTemplateImageSourcesBelongToTheTemplateImageOnly(t *testing.T) {
	spec := store.InferenceSpecRow{Image: imageSourcesUpstream}
	selection := templateSelection{Spec: &spec, ImageSources: []string{imageSourcesSWR}}
	if got := templateImageSources(selection, imageSourcesUpstream); !reflect.DeepEqual(got, []string{imageSourcesSWR}) {
		t.Fatalf("sources of the template image = %v", got)
	}
	if got := templateImageSources(selection, strings.ToUpper(imageSourcesUpstream[:4])+imageSourcesUpstream[4:]); len(got) != 1 {
		t.Fatalf("the image is compared case-insensitively like the trusted override: %v", got)
	}
	for _, image := range []string{"", "custom/vllm:cuda", "ghcr.io/org/runner@sha256:bbbb"} {
		if got := templateImageSources(selection, image); got != nil {
			t.Errorf("sources of %q = %v, want none", image, got)
		}
	}
	if got := templateImageSources(templateSelection{ImageSources: []string{"x"}}, "x"); got != nil {
		t.Fatalf("no template, no sources: %v", got)
	}
}

// The selection the template defaults produce carries the spec's sources and
// leaves an unrelated request alone: the template identity stays the image.
func TestApplyTemplateDefaultsReadsTheSpecsImageSources(t *testing.T) {
	rig := newImageSourcesRig(t, "defaults")
	req := dto.DeployRequest{TemplateID: rig.template.ID}
	selection, err := rig.h.applyTemplateDefaults(&req, templateHardware{GPU: chipmatch.GPU{Name: "H100", Vendor: "nvidia", CC: 9.0}})
	if err != nil {
		t.Fatal(err)
	}
	if req.Image != imageSourcesUpstream || !reflect.DeepEqual(selection.ImageSources, []string{imageSourcesSWR, imageSourcesNJU}) || selection.Deviated {
		t.Fatalf("image=%q sources=%v deviated=%v", req.Image, selection.ImageSources, selection.Deviated)
	}
	snapshot := snapshotTemplateSelection(selection, req.Image)
	if !reflect.DeepEqual(snapshot.ImageSources, selection.ImageSources) {
		t.Fatalf("snapshot sources = %v", snapshot.ImageSources)
	}
	if overridden := snapshotTemplateSelection(selection, "custom/vllm:cuda"); overridden.ImageSources != nil {
		t.Fatalf("an overridden image snapshotted the template's sources: %v", overridden.ImageSources)
	}
}

// A stored image_sources column that does not decode is reported with the spec's
// name rather than silently dropping the sources.
func TestApplyTemplateDefaultsRefusesUndecodableImageSources(t *testing.T) {
	rig := newImageSourcesRig(t, "defaults_corrupt")
	if err := rig.st.DB().Model(&store.InferenceSpecRow{}).Where("name = ?", "fp8-balanced").Update("image_sources", "not json").Error; err != nil {
		t.Fatal(err)
	}
	req := dto.DeployRequest{TemplateID: rig.template.ID}
	_, err := rig.h.applyTemplateDefaults(&req, templateHardware{GPU: chipmatch.GPU{Name: "H100", Vendor: "nvidia", CC: 9.0}})
	if err == nil || !strings.Contains(err.Error(), "inference spec fp8-balanced image_sources") {
		t.Fatalf("error = %v", err)
	}
}
