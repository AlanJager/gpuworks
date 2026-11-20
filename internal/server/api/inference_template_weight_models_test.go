package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func weightModelsTemplate(t *testing.T, f *sourceIDFixture, sources string) uint {
	t.Helper()
	catalog := deploymentFlatFixture()
	catalog.Specs[0].WeightSources = sources
	if err := f.st.ReconcileInferenceCatalog([]store.InferenceModel{catalog}); err != nil {
		t.Fatal(err)
	}
	template, err := f.st.InferenceModelByModelID(catalog.ModelID)
	if err != nil {
		t.Fatal(err)
	}
	return template.ID
}

func (f *sourceIDFixture) get(path string, authorized bool) *httptest.ResponseRecorder {
	f.t.Helper()
	request := httptest.NewRequest(http.MethodGet, path, nil)
	if authorized {
		request.Header.Set("Authorization", "Bearer "+f.token)
	}
	recorder := httptest.NewRecorder()
	f.router.ServeHTTP(recorder, request)
	return recorder
}

func weightModelsPath(templateID uint) string {
	return fmt.Sprintf("/api/v1/inference-templates/weight-models?template_id=%d&spec_source=upstream&spec_name=fp8-balanced", templateID)
}

// The repository id is the identity: a provider is a mirror and a revision a
// point in history, so neither keeps a model from serving a source; a
// different repository, or a subfolder-only copy asked for the whole
// repository, never serves it.
func TestTemplateWeightModelsMatchesByRepositoryIdentity(t *testing.T) {
	f := newSourceIDFixture(t)
	templateID := weightModelsTemplate(t, f, `[{"provider":"huggingface","repo_id":"Qwen/Qwen3-1.7B","revision":"70d244cc","subfolder":"weights"},{"provider":"modelscope","repo_id":"Acme/X-Int8"}]`)
	serving := map[string]*store.Model{
		"exact":             f.model(&store.Model{Source: "huggingface", SourceID: "qwen/qwen3-1.7b", Revision: "70d244cc", ArtifactSubfolder: "weights"}),
		"whole repository":  f.model(&store.Model{Source: "huggingface", SourceID: "Qwen/Qwen3-1.7B", Revision: "main"}),
		"never pinned":      f.model(&store.Model{Source: "huggingface", SourceID: "Qwen/Qwen3-1.7B", Revision: "", ArtifactSubfolder: "weights"}),
		"other provider":    f.model(&store.Model{Source: "modelscope", SourceID: "Qwen/Qwen3-1.7B", Revision: "v1", ArtifactSubfolder: "weights"}),
		"renamed local":     f.model(&store.Model{Source: "local", SourceID: "Qwen/Qwen3-1.7B", Revision: "local"}),
		"quantized exactly": f.model(&store.Model{Source: "huggingface", SourceID: "Acme/X-Int8", Revision: "abc"}),
	}
	excluded := map[string]*store.Model{
		"base of a quantized repository": f.model(&store.Model{Source: "huggingface", SourceID: "Acme/X", Revision: "abc"}),
		"subfolder-only copy":            f.model(&store.Model{Source: "modelscope", SourceID: "Acme/X-Int8", Revision: "abc", ArtifactSubfolder: "weights"}),
		"other subfolder":                f.model(&store.Model{Source: "huggingface", SourceID: "Qwen/Qwen3-1.7B", Revision: "main", ArtifactSubfolder: "other"}),
	}

	rec := f.get(weightModelsPath(templateID), true)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	var resp struct {
		Models []struct {
			ID       uint            `json:"id"`
			Source   string          `json:"source"`
			Presence json.RawMessage `json:"presence"`
			README   *string         `json:"readme"`
		} `json:"models"`
		Sources []map[string]any `json:"sources"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil {
		t.Fatal(err)
	}
	got := map[uint]bool{}
	for _, m := range resp.Models {
		if got[m.ID] {
			t.Fatalf("model %d listed twice: %s", m.ID, rec.Body.String())
		}
		got[m.ID] = true
		if len(m.Presence) == 0 || m.README != nil {
			t.Fatalf("model row shape: %s", rec.Body.String())
		}
	}
	for name, m := range serving {
		if !got[m.ID] {
			t.Errorf("%s (model %d) does not serve the spec: %s", name, m.ID, rec.Body.String())
		}
	}
	for name, m := range excluded {
		if got[m.ID] {
			t.Errorf("%s (model %d) was offered: %s", name, m.ID, rec.Body.String())
		}
	}
	if len(resp.Models) != len(serving) || resp.Models[0].ID != serving["quantized exactly"].ID {
		t.Fatalf("models must be the serving ones, newest first: %s", rec.Body.String())
	}
	// Each declared source, in order, names the newest same-provider model a
	// choice of it reuses.
	want := []map[string]any{
		{"provider": "huggingface", "repo_id": "Qwen/Qwen3-1.7B", "revision": "70d244cc", "subfolder": "weights", "reuse_model_id": float64(serving["never pinned"].ID)},
		{"provider": "modelscope", "repo_id": "Acme/X-Int8"},
	}
	if fmt.Sprint(resp.Sources) != fmt.Sprint(want) {
		t.Fatalf("sources = %v, want %v", resp.Sources, want)
	}
}

func TestTemplateWeightModelsWithNothingRegisteredListsNone(t *testing.T) {
	f := newSourceIDFixture(t)
	templateID := weightModelsTemplate(t, f, `[{"provider":"modelscope","repo_id":"Acme/Deploy-FP8"}]`)
	rec := f.get(weightModelsPath(templateID), true)
	if rec.Code != http.StatusOK || strings.TrimSpace(rec.Body.String()) != `{"models":[],"sources":[{"provider":"modelscope","repo_id":"Acme/Deploy-FP8"}]}` {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
}

func TestTemplateWeightModelsRowsCarryPresence(t *testing.T) {
	f := newSourceIDFixture(t)
	templateID := weightModelsTemplate(t, f, `[{"provider":"modelscope","repo_id":"Acme/Deploy-FP8"}]`)
	model := f.model(&store.Model{Source: "modelscope", SourceID: "Acme/Deploy-FP8", Revision: "main"})
	node, err := f.st.RegisterNode("weights-node", store.HashToken("weights-token"), "weights-node")
	if err != nil {
		t.Fatal(err)
	}
	if _, err := f.st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, NodeID: node.ID, State: proto.MFPresent, Path: "/models/x"}); err != nil {
		t.Fatal(err)
	}
	rec := f.get(weightModelsPath(templateID), true)
	var resp struct {
		Models []struct {
			Presence struct {
				ReadyNodes int `json:"ready_nodes"`
			} `json:"presence"`
		} `json:"models"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &resp); err != nil || len(resp.Models) != 1 || resp.Models[0].Presence.ReadyNodes != 1 {
		t.Fatalf("status=%d body=%s err=%v", rec.Code, rec.Body.String(), err)
	}
}

func TestTemplateWeightModelsRefusals(t *testing.T) {
	f := newSourceIDFixture(t)
	templateID := weightModelsTemplate(t, f, `[{"provider":"modelscope","repo_id":"Acme/Deploy-FP8"}]`)
	for _, query := range []string{
		"",
		"template_id=x&spec_source=upstream&spec_name=fp8-balanced",
		"template_id=0&spec_source=upstream&spec_name=fp8-balanced",
		fmt.Sprintf("template_id=%d&spec_name=fp8-balanced", templateID),
		fmt.Sprintf("template_id=%d&spec_source=upstream", templateID),
	} {
		rec := f.get("/api/v1/inference-templates/weight-models?"+query, true)
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("query %q: status=%d body=%s", query, rec.Code, rec.Body.String())
		}
		if code, _ := sourceIDErrorCode(t, rec); code != ErrorTemplateWeightModelsQueryInvalid {
			t.Fatalf("query %q: code=%s", query, code)
		}
	}
	for _, path := range []string{
		fmt.Sprintf("/api/v1/inference-templates/weight-models?template_id=%d&spec_source=user&spec_name=fp8-balanced", templateID),
		fmt.Sprintf("/api/v1/inference-templates/weight-models?template_id=%d&spec_source=upstream&spec_name=missing", templateID),
		"/api/v1/inference-templates/weight-models?template_id=9999&spec_source=upstream&spec_name=fp8-balanced",
	} {
		if rec := f.get(path, true); rec.Code != http.StatusNotFound {
			t.Fatalf("%s: status=%d body=%s", path, rec.Code, rec.Body.String())
		}
	}
	if rec := f.get(weightModelsPath(templateID), false); rec.Code != http.StatusUnauthorized {
		t.Fatalf("anonymous status=%d", rec.Code)
	}
	request := httptest.NewRequest(http.MethodPost, weightModelsPath(templateID), nil)
	request.Header.Set("Authorization", "Bearer "+f.token)
	recorder := httptest.NewRecorder()
	f.router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusMethodNotAllowed {
		t.Fatalf("POST status=%d", recorder.Code)
	}
}

// A rank-scoped spec's ranks are one pre-split set of files of one revision.
// The form offers rank 0's artifact at the revision the template states, as
// the single registered model a choice of the source reuses: the other ranks'
// rows (a deployment's own model must be rank 0's) and the rows of a revision
// the template no longer states are not choices.
func TestTemplateWeightModelsOffersOnlyRankZeroAtTheStatedRevision(t *testing.T) {
	f := newSourceIDFixture(t)
	templateID := weightModelsTemplate(t, f, `[{"provider":"modelscope","repo_id":"Acme/Ranked","revision":"new","subfolder":"rank${GPUWORKS_NODE_RANK}"}]`)
	for rank := range 4 {
		f.model(&store.Model{Source: "modelscope", SourceID: "Acme/Ranked", Revision: "old", ArtifactSubfolder: fmt.Sprintf("rank%d", rank)})
	}
	ask := func() (models []uint, reuse uint) {
		t.Helper()
		rec := f.get(weightModelsPath(templateID), true)
		var resp struct {
			Models  []struct{ ID uint } `json:"models"`
			Sources []struct {
				ReuseModelID uint `json:"reuse_model_id"`
			} `json:"sources"`
		}
		if rec.Code != http.StatusOK || json.Unmarshal(rec.Body.Bytes(), &resp) != nil || len(resp.Sources) != 1 {
			t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
		}
		for _, model := range resp.Models {
			models = append(models, model.ID)
		}
		return models, resp.Sources[0].ReuseModelID
	}
	if models, reuse := ask(); len(models) != 0 || reuse != 0 {
		t.Fatalf("only revision old is registered, but the form is offered %v (reuse %d)", models, reuse)
	}
	var current []*store.Model
	for rank := range 4 {
		current = append(current, f.model(&store.Model{Source: "modelscope", SourceID: "Acme/Ranked", Revision: "new", ArtifactSubfolder: fmt.Sprintf("rank%d", rank)}))
	}
	if models, reuse := ask(); fmt.Sprint(models) != fmt.Sprint([]uint{current[0].ID}) || reuse != current[0].ID {
		t.Fatalf("form offers %v (reuse %d), want only rank 0's artifact %d", models, reuse, current[0].ID)
	}
}
