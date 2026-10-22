package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/modelmeta"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The sizes below are the real ones of ISTA-DASLab/Qwen3.8-Flash-Next-GSQ-RCO-GGUF,
// the repository that holds one GGUF quantization per directory.
const (
	subfolderIQ2XSBytes = int64(39225954592 + 28800138432) // 63.35 GiB
	subfolderWholeBytes = subfolderIQ2XSBytes + 54817524224 + 28800138432 + 37623740192 + 28800138432 + 907543008
)

var quantizationRepo = []struct {
	path string
	size int64
}{
	{"IQ2_XS/Qwen3.8-Flash-Next-GSQ-RCO-IQ2_XS-00001-of-00002.gguf", 39225954592},
	{"IQ2_XS/Qwen3.8-Flash-Next-GSQ-RCO-IQ2_XS-00002-of-00002.gguf", 28800138432},
	{"IQ3_S/Qwen3.8-Flash-Next-GSQ-RCO-IQ3_S-00001-of-00002.gguf", 54817524224},
	{"IQ3_S/Qwen3.8-Flash-Next-GSQ-RCO-IQ3_S-00002-of-00002.gguf", 28800138432},
	{"Q2_0/Qwen3.8-Flash-Next-GSQ-RCO-Q2_0-00001-of-00002.gguf", 37623740192},
	{"Q2_0/Qwen3.8-Flash-Next-GSQ-RCO-Q2_0-00002-of-00002.gguf", 28800138432},
	{"README.md", 15693},
	{"mmproj-Qwen3.8-Flash-Next-BF16.gguf", 907543008},
}

// quantizationHub answers the repository listing of both hubs for a repository
// with several quantizations and no config.json, and 404s everything else as
// the real hosts do.
func quantizationHub(t *testing.T) *httptest.Server {
	t.Helper()
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/json")
		switch r.URL.Path {
		case "/api/models/ISTA-DASLab/Quants":
			_, _ = w.Write([]byte(`{"author":"ISTA-DASLab","createdAt":"2026-09-01T00:00:00Z","lastModified":"2026-09-20T00:00:00Z"}`))
		case "/api/v1/models/ISTA-DASLab/Quants":
			_, _ = w.Write([]byte(`{"Code":200,"Data":{"Avatar":"","CreatedTime":1788220800,"LastUpdatedTime":1789862400}}`))
		case "/api/models/ISTA-DASLab/Quants/tree/main":
			items := []map[string]any{{"type": "directory", "oid": "a2d97c5b", "size": 0, "path": "IQ2_XS"}}
			for _, f := range quantizationRepo {
				items = append(items, map[string]any{"type": "file", "oid": "c4f19e7d", "size": f.size, "path": f.path})
			}
			_ = json.NewEncoder(w).Encode(items)
		case "/api/v1/models/ISTA-DASLab/Quants/repo/files":
			files := []map[string]any{{"Type": "tree", "Name": "IQ2_XS", "Path": "IQ2_XS", "Size": 0, "Revision": "1868010e27505d4b8cc15970cdcc4dd76005d7de", "CommittedDate": 1789499531}}
			for _, f := range quantizationRepo {
				files = append(files, map[string]any{"Type": "blob", "Name": f.path[strings.LastIndex(f.path, "/")+1:], "Path": f.path, "Size": f.size,
					"Revision": "1868010e27505d4b8cc15970cdcc4dd76005d7de", "CommittedDate": 1789499531})
			}
			_ = json.NewEncoder(w).Encode(map[string]any{"Code": 200, "Success": true, "Data": map[string]any{
				"LatestCommitter": map[string]any{"CommittedDate": 1789499531, "ShortId": "1868010e"}, "Files": files}})
		default:
			http.NotFound(w, r)
		}
	}))
	t.Cleanup(srv.Close)
	return srv
}

// fetchFrom is the production metadata fetcher pointed at a fixture hub.
func fetchFrom(hub *httptest.Server) func(context.Context, string, string, string, string, []string) (*modelmeta.Meta, error) {
	return func(ctx context.Context, source, repo, revision, subfolder string, _ []string) (*modelmeta.Meta, error) {
		return modelmeta.Fetch(ctx, source, repo, revision, subfolder, hub.URL)
	}
}

func waitModelWeight(t *testing.T, st *store.Store, id uint, want int64) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if model, err := st.GetModel(id); err == nil && model.WeightBytes == want {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	model, err := st.GetModel(id)
	t.Fatalf("model weight bytes = %v (err=%v), want %d", func() any {
		if model == nil {
			return nil
		}
		return model.WeightBytes
	}(), err, want)
}

// The deploy form registers a template's weight source with its subfolder and
// the background refresh must size that quantization, not the repository: the
// whole repository is 274.57 GiB, which fails disk admission on a node that
// has room for the 63.35 GiB it will actually download.
func TestQuickRegisterSubfolderRefreshStoresTheSubfolderWeightSize(t *testing.T) {
	hub := quantizationHub(t)
	for _, source := range []string{"huggingface", "modelscope"} {
		t.Run(source, func(t *testing.T) {
			st, err := store.Open("file:api_quick_register_subfolder_"+source+"?mode=memory&cache=shared", bus.New())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			h := apiHandlers{d: Deps{Store: st, FetchModelMeta: fetchFrom(hub)}}
			revision := "main"
			if source == "modelscope" {
				revision = "1868010e27505d4b8cc15970cdcc4dd76005d7de"
			}
			register := func(subfolder string) dto.Model {
				body := fmt.Sprintf(`{"source":%q,"source_id":"ISTA-DASLab/Quants","revision":%q,"artifact_subfolder":%q}`, source, revision, subfolder)
				rec := httptest.NewRecorder()
				h.quickRegisterModel(rec, httptest.NewRequest(http.MethodPost, "/api/v1/models/quick-register", strings.NewReader(body)))
				if rec.Code != http.StatusCreated {
					t.Fatalf("quick register %q = %d %s", subfolder, rec.Code, rec.Body.String())
				}
				var wire dto.Model
				if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil {
					t.Fatal(err)
				}
				return wire
			}
			scoped := register("IQ2_XS")
			whole := register("")
			if scoped.ArtifactSubfolder != "IQ2_XS" {
				t.Fatalf("registered artifact subfolder = %q", scoped.ArtifactSubfolder)
			}
			waitModelWeight(t, st, scoped.ID, subfolderIQ2XSBytes)
			// The same repository registered whole is a different artifact and
			// keeps the repository-wide size.
			waitModelWeight(t, st, whole.ID, subfolderWholeBytes)
		})
	}
}

// Refreshing a registered subfolder model repairs a size an earlier refresh
// stored from the whole repository.
func TestRefreshModelMetadataKeepsTheSubfolderScope(t *testing.T) {
	hub := quantizationHub(t)
	st, err := store.Open("file:api_refresh_subfolder_scope?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "ISTA-DASLab/Quants", Revision: "main", ArtifactSubfolder: "IQ2_XS",
		Name: "quants", WeightBytes: subfolderWholeBytes})
	if err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st, FetchModelMeta: fetchFrom(hub)}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/models/1/metadata/refresh", strings.NewReader(`{}`))
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": fmt.Sprint(model.ID)}))
	rec := httptest.NewRecorder()
	h.refreshModelMetadata(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
	}
	updated, err := st.GetModel(model.ID)
	if err != nil || updated.WeightBytes != subfolderIQ2XSBytes {
		t.Fatalf("weight bytes = %d err=%v, want %d", updated.WeightBytes, err, subfolderIQ2XSBytes)
	}
}

// The browser can only size a whole repository, so a browser-supplied size
// must not replace the stored size of a model registered from a subfolder.
func TestBrowserMetadataRefreshDoesNotReplaceASubfolderWeightSize(t *testing.T) {
	st, err := store.Open("file:api_refresh_subfolder_browser?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "ISTA-DASLab/Quants", Revision: "main", ArtifactSubfolder: "IQ2_XS",
		Name: "quants", WeightBytes: subfolderIQ2XSBytes})
	if err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st}}
	body := fmt.Sprintf(`{"readme":"# quants","weight_bytes":%d,"browser_metadata":true}`, subfolderWholeBytes)
	req := httptest.NewRequest(http.MethodPost, "/api/v1/models/1/metadata/refresh", strings.NewReader(body))
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": fmt.Sprint(model.ID)}))
	rec := httptest.NewRecorder()
	h.refreshModelMetadata(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
	}
	updated, err := st.GetModel(model.ID)
	if err != nil || updated.WeightBytes != subfolderIQ2XSBytes || updated.README != "# quants" {
		t.Fatalf("model = %+v err=%v, want the stored subfolder size kept and the README refreshed", updated, err)
	}
}

// Importing a template that omits weight_bytes backfills it from the declared
// source; for a subfolder source that is the subfolder's size.
func TestInferenceTemplateImportBackfillsSubfolderWeightBytes(t *testing.T) {
	hub := quantizationHub(t)
	st, err := store.Open("file:flat_api_weight_backfill_subfolder?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	h := &apiHandlers{d: Deps{Store: st, FetchModelMeta: fetchFrom(hub)}}
	yaml := `models:
- model_id: acme/quants
  specs:
  - name: default
    engine: vllm
    image: vllm:latest
    weights:
      quantization: IQ2_XS
      sources:
      - huggingface:
          repo_id: ISTA-DASLab/Quants
          revision: main
          subfolder: IQ2_XS
    gpus: {vendor: nvidia}
`
	recorder := httptest.NewRecorder()
	h.importInferenceTemplates(recorder, httptest.NewRequest(http.MethodPost, "/api/v1/inference-templates/import", strings.NewReader(yaml)))
	if recorder.Code != http.StatusOK {
		t.Fatalf("status/body=%d %s", recorder.Code, recorder.Body.String())
	}
	model, err := st.InferenceModelByModelID("acme/quants")
	if err != nil || len(model.Specs) != 1 || model.Specs[0].WeightBytes != subfolderIQ2XSBytes {
		t.Fatalf("model=%+v err=%v, want weight bytes %d", model, err, subfolderIQ2XSBytes)
	}
}
