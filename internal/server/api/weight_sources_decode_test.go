package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/acquisition"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// weightSourcesColumnValues are the shapes an inference_specs.weight_sources
// value can take, keyed by a short name. "partial" decodes its first element
// and fails on the second, which encoding/json still appends with the fields
// it could read.
var weightSourcesColumnValues = []struct{ name, raw string }{
	{"blank", ""},
	{"whitespace", "  "},
	{"null", "null"},
	{"empty", "[]"},
	{"undecodable", "not json"},
	{"partial", `[{"provider":"huggingface","repo_id":"org/A"},{"provider":"modelscope","repo_id":5}]`},
	{"valid", `[{"provider":"huggingface","repo_id":"org/A","revision":"r1"},{"provider":"modelscope","repo_id":"org/A-ms"}]`},
}

// The api reads a spec's declared weight sources in several places, each with
// its own tolerance for a blank or unreadable column. These pin what each one
// makes of every shape, as wire bytes where the result reaches a response.
func TestSpecWeightSourcesReadersKeepTheirHandlingOfEveryColumnShape(t *testing.T) {
	wantSpecSources := map[string]string{
		"blank": `null`, "whitespace": `null`, "null": `null`, "empty": `[]`, "undecodable": `null`,
		"partial": `[{"provider":"huggingface","repo_id":"org/A"},{"provider":"modelscope","repo_id":""}]`,
		"valid":   `[{"provider":"huggingface","repo_id":"org/A","revision":"r1"},{"provider":"modelscope","repo_id":"org/A-ms"}]`,
	}
	partialProviders := []acquisition.WeightSource{{Provider: "huggingface", RepoID: "org/A", Suggested: true}, {Provider: "modelscope"}}
	wantProviders := map[string][]acquisition.WeightSource{
		"blank": {}, "whitespace": {}, "null": {}, "empty": {}, "undecodable": {},
		"partial": partialProviders,
		"valid":   {{Provider: "huggingface", RepoID: "org/A", Revision: "r1", Suggested: true}, {Provider: "modelscope", RepoID: "org/A-ms"}},
	}
	for _, value := range weightSourcesColumnValues {
		t.Run(value.name, func(t *testing.T) {
			spec := &store.InferenceSpecRow{Name: "s", WeightSources: value.raw}

			encoded, err := json.Marshal(specWeightSources(spec))
			if err != nil || string(encoded) != wantSpecSources[value.name] {
				t.Fatalf("specWeightSources = %s, %v; want %s", encoded, err, wantSpecSources[value.name])
			}

			selection := templateSelection{Spec: spec, SuggestedWeight: weightSource{Provider: "huggingface", RepoID: "org/A"}}
			if got := templateAcquisitionProviders(selection); got == nil || !reflect.DeepEqual(got, wantProviders[value.name]) {
				t.Fatalf("templateAcquisitionProviders = %#v, want %#v", got, wantProviders[value.name])
			}

			view := inferenceTemplateView(store.InferenceModel{ModelID: "org/a", Specs: []store.InferenceSpecRow{*spec}})
			encoded, err = json.Marshal(view.Specs[0].WeightSources)
			if err != nil || string(encoded) != wantSpecSources[value.name] {
				t.Fatalf("template view weight_sources = %s, %v; want %s", encoded, err, wantSpecSources[value.name])
			}
		})
	}
	if got := specWeightSources(nil); got != nil {
		t.Fatalf("specWeightSources(nil) = %#v, want nil", got)
	}
}

// The template index lists each profile's declared repositories; a profile
// whose column yields none carries no weight_repo_ids key at all.
func TestInferenceTemplateIndexWeightRepoIDsForEveryColumnShape(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	seedWeightSourcesColumnValues(t, st)
	rec := httptest.NewRecorder()
	(&apiHandlers{d: Deps{Store: st}}).listInferenceTemplateIndex(rec, httptest.NewRequest(http.MethodGet, "/api/v1/inference-templates/index", nil))
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
	}
	var wire struct {
		Items []struct {
			Profiles []struct {
				Name          string           `json:"name"`
				WeightRepoIDs *json.RawMessage `json:"weight_repo_ids"`
			} `json:"profiles"`
		} `json:"items"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil || len(wire.Items) != 1 {
		t.Fatalf("index = %s, %v", rec.Body, err)
	}
	got := map[string]string{}
	for _, profile := range wire.Items[0].Profiles {
		got[profile.Name] = "absent"
		if profile.WeightRepoIDs != nil {
			got[profile.Name] = string(*profile.WeightRepoIDs)
		}
	}
	want := map[string]string{
		"blank": "absent", "whitespace": "absent", "null": "absent", "empty": "absent", "undecodable": "absent",
		"partial": `["org/A"]`, "valid": `["org/A","org/A-ms"]`,
	}
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("weight_repo_ids = %v, want %v", got, want)
	}
}

// An explicit public provider is looked up among the declared sources. A column
// that does not decode is an error; the selection is left untouched.
func TestSelectExplicitWeightRejectsAnUndecodableColumn(t *testing.T) {
	h := &apiHandlers{d: Deps{Store: newAcquisitionStore(t)}}
	for _, value := range weightSourcesColumnValues {
		if value.name != "undecodable" && value.name != "partial" {
			continue
		}
		t.Run(value.name, func(t *testing.T) {
			var want []weightSource
			wantErr := json.Unmarshal([]byte(value.raw), &want)
			selection := templateSelection{Spec: &store.InferenceSpecRow{WeightSources: value.raw}}
			err := h.selectExplicitWeight(&selection, &dto.ModelSource{Provider: "modelscope"})
			if err == nil || err.Error() != wantErr.Error() || selection.ExplicitProvider || selection.Weight != (weightSource{}) {
				t.Fatalf("selectExplicitWeight = %v, selection %+v; want %v and no selection", err, selection, wantErr)
			}
		})
	}
}

// A spec whose weight_sources column is blank or unreadable cannot launch. The
// request fails at the declared-source selection, before an explicit provider
// is considered, with the text the deploy and evaluate handlers return as 400.
func TestApplyTemplateDefaultsRejectsAnUnusableWeightSourcesColumn(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	seedWeightSourcesColumnValues(t, st)
	stored, err := st.InferenceModelByModelID("org/a")
	if err != nil {
		t.Fatal(err)
	}
	h := &apiHandlers{d: Deps{Store: st}}
	for name, want := range map[string]string{
		// A blank column declares no sources (inferencecatalog.DecodeWeightSources),
		// so it reads as one: before the shared decoder it surfaced encoding/json's
		// "weight sources: unexpected end of JSON input" instead.
		"blank":       "inference spec blank has no weight source",
		"whitespace":  "inference spec whitespace has no weight source",
		"null":        "inference spec null has no weight source",
		"empty":       "inference spec empty has no weight source",
		"undecodable": "inference spec undecodable weight sources: invalid character 'o' in literal null (expecting 'u')",
		"partial":     "inference spec partial weight sources: json: cannot unmarshal number into Go struct field WeightSource.repo_id of type string",
	} {
		t.Run(name, func(t *testing.T) {
			req := &dto.DeployRequest{TemplateID: stored.ID, TemplateSpecName: name,
				ModelSource: &dto.ModelSource{Provider: "modelscope"}}
			_, err := h.applyTemplateDefaults(req, templateHardware{})
			var productErr interface{ ProductCode() string }
			if err == nil || err.Error() != want || errors.As(err, &productErr) {
				t.Fatalf("applyTemplateDefaults = %v, want %q", err, want)
			}
		})
	}
}

// seedWeightSourcesColumnValues stores catalog model org/a with one spec per
// column shape, named after it. The store refuses a blank or malformed column
// on write, so those are put in place underneath it, as a damaged database
// would hold them.
func seedWeightSourcesColumnValues(t *testing.T, st *store.Store) {
	t.Helper()
	model := flatAPIFixture("org/a", "unused")
	model.Specs = nil
	for _, value := range weightSourcesColumnValues {
		model.Specs = append(model.Specs, flatAPIFixture("org/a", value.name).Specs[0])
	}
	if err := st.ReconcileInferenceCatalog([]store.InferenceModel{model}); err != nil {
		t.Fatal(err)
	}
	for _, value := range weightSourcesColumnValues {
		if err := st.DB().Model(&store.InferenceSpecRow{}).Where("name = ?", value.name).Update("weight_sources", value.raw).Error; err != nil {
			t.Fatal(err)
		}
	}
}
