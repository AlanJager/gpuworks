package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/modelmeta"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/pkg/modelconfig"
)

// An upstream that cannot tell us which commit its default branch points at
// must still produce a usable registration, with the unpinned identity intact.
func TestCreateModelPreservesEmptyRevisionWhenUpstreamResolvesNothing(t *testing.T) {
	st, err := store.Open("file:api_create_model_empty_revision?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	fetchedRevision := "not-called"
	h := apiHandlers{d: Deps{
		Store: st,
		FetchModelMeta: func(_ context.Context, _, _, revision, _ string, _ []string) (*modelmeta.Meta, error) {
			fetchedRevision = revision
			return &modelmeta.Meta{}, nil
		},
	}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/models", strings.NewReader(`{"source":"modelscope","source_id":"org/model","revision":"   ","name":"model"}`))
	rec := httptest.NewRecorder()
	h.createModel(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
	}
	models, err := st.ListModels()
	if err != nil || len(models) != 1 {
		t.Fatalf("models=%+v err=%v", models, err)
	}
	if models[0].Revision != "" || fetchedRevision != "" {
		t.Fatalf("stored revision=%q fetched revision=%q, want empty", models[0].Revision, fetchedRevision)
	}
}

// Registering without a revision pins the commit the default branch points at,
// so every later download and peer sync moves the same bytes. Re-registering
// the same repository while upstream still resolves to that commit must reuse
// the existing row instead of creating a duplicate identity.
func TestCreateModelPinsResolvedDefaultBranchRevisionIdempotently(t *testing.T) {
	st, err := store.Open("file:api_create_model_pins_revision?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	const commit = "4d58a7b524cd0a41f80a7fbb6a5b22f2fca1e3c9"
	requestedRevisions := []string{}
	h := apiHandlers{d: Deps{
		Store: st,
		FetchModelMeta: func(_ context.Context, _, _, revision, _ string, _ []string) (*modelmeta.Meta, error) {
			requestedRevisions = append(requestedRevisions, revision)
			return &modelmeta.Meta{ResolvedRevision: commit, Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 30}, nil
		},
	}}
	for attempt := 0; attempt < 2; attempt++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/models", strings.NewReader(`{"source":"modelscope","source_id":"Qwen/Qwen3.5-0.8B"}`))
		rec := httptest.NewRecorder()
		h.createModel(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("attempt %d status/body = %d %s", attempt, rec.Code, rec.Body.String())
		}
		var wire map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil {
			t.Fatal(err)
		}
		if wire["revision"] != commit {
			t.Fatalf("attempt %d response revision=%v, want %q", attempt, wire["revision"], commit)
		}
	}
	models, err := st.ListModels()
	if err != nil || len(models) != 1 {
		t.Fatalf("models=%+v err=%v, want a single row", models, err)
	}
	if models[0].Revision != commit || models[0].SourceID != "Qwen/Qwen3.5-0.8B" {
		t.Fatalf("stored model=%+v", models[0])
	}
	if len(requestedRevisions) != 2 || requestedRevisions[0] != "" || requestedRevisions[1] != "" {
		t.Fatalf("metadata fetch revisions=%v, want the unpinned request both times", requestedRevisions)
	}
}

// A user-chosen revision is an explicit decision and is never replaced by
// whatever the default branch happens to point at.
func TestCreateModelKeepsAnExplicitRevisionOverTheResolvedOne(t *testing.T) {
	st, err := store.Open("file:api_create_model_keeps_explicit_revision?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := apiHandlers{d: Deps{Store: st, FetchModelMeta: func(_ context.Context, _, _, revision, _ string, _ []string) (*modelmeta.Meta, error) {
		if revision != "v1.2" {
			t.Fatalf("metadata revision=%q, want the requested ref", revision)
		}
		return &modelmeta.Meta{ResolvedRevision: "1111111111111111111111111111111111111111"}, nil
	}}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/models", strings.NewReader(`{"source":"huggingface","source_id":"acme/model","revision":"v1.2"}`))
	rec := httptest.NewRecorder()
	h.createModel(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
	}
	models, err := st.ListModels()
	if err != nil || len(models) != 1 || models[0].Revision != "v1.2" {
		t.Fatalf("models=%+v err=%v", models, err)
	}
}

func TestQuickRegisterModelIsImmediateIdempotentAndRefreshesInBackground(t *testing.T) {
	st, err := store.Open("file:api_quick_register_model?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	started := make(chan struct{}, 1)
	h := apiHandlers{d: Deps{Store: st, FetchModelMeta: func(ctx context.Context, source, repo, revision, _ string, candidates []string) (*modelmeta.Meta, error) {
		started <- struct{}{}
		if source != "modelscope" || repo != "Org/Model" || revision != "master" || strings.Join(candidates, ",") != "https://modelscope.cn" {
			t.Errorf("metadata request = %q/%q/%q/%q", source, repo, revision, candidates)
		}
		return &modelmeta.Meta{WeightBytes: 2 << 30, Arch: "Qwen3ForCausalLM"}, nil
	}}}
	body := `{"source":"modelscope","source_id":"Org/Model","revision":"main","quantization":"int8","architecture":"Qwen3","context_length":4096}`
	rec := httptest.NewRecorder()
	h.quickRegisterModel(rec, httptest.NewRequest(http.MethodPost, "/api/v1/models/quick-register", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
	}
	var wire dto.Model
	if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil {
		t.Fatal(err)
	}
	if wire.Source != "modelscope" || wire.Revision != "master" || wire.WeightGiB != 0 {
		t.Fatalf("quick registration response = %+v", wire)
	}
	select {
	case <-started:
	case <-time.After(2 * time.Second):
		t.Fatal("background metadata refresh did not start")
	}
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		model, getErr := st.GetModel(wire.ID)
		if getErr == nil && model.WeightBytes == 2<<30 && model.Arch == "Qwen3ForCausalLM" {
			break
		}
		time.Sleep(10 * time.Millisecond)
	}
	model, err := st.GetModel(wire.ID)
	if err != nil || model.WeightBytes != 2<<30 || model.Arch != "Qwen3ForCausalLM" {
		t.Fatalf("refreshed model=%+v err=%v", model, err)
	}
	duplicate := httptest.NewRecorder()
	h.quickRegisterModel(duplicate, httptest.NewRequest(http.MethodPost, "/api/v1/models/quick-register", strings.NewReader(body)))
	if duplicate.Code != http.StatusOK {
		t.Fatalf("duplicate status/body = %d %s", duplicate.Code, duplicate.Body.String())
	}
	models, err := st.ListModels()
	if err != nil || len(models) != 1 {
		t.Fatalf("models=%+v err=%v", models, err)
	}
}

func TestQuickRegisterModelValidatesSourceAndSourceID(t *testing.T) {
	st, err := store.Open("file:api_quick_register_validation?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := apiHandlers{d: Deps{Store: st}}
	for _, body := range []string{`{}`, `{"source":"local","source_id":"org/model"}`} {
		rec := httptest.NewRecorder()
		h.quickRegisterModel(rec, httptest.NewRequest(http.MethodPost, "/api/v1/models/quick-register", strings.NewReader(body)))
		if rec.Code != http.StatusBadRequest {
			t.Fatalf("body=%s status=%d response=%s", body, rec.Code, rec.Body.String())
		}
	}
}

// A metadata fetch failure is best-effort: registration still succeeds and the
// model stays unpinned rather than half-populated.
func TestCreateModelSucceedsUnpinnedWhenMetadataFetchFails(t *testing.T) {
	st, err := store.Open("file:api_create_model_metadata_error?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := apiHandlers{d: Deps{Store: st, FetchModelMeta: func(context.Context, string, string, string, string, []string) (*modelmeta.Meta, error) {
		return nil, fmt.Errorf("dial tcp: i/o timeout")
	}}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/models", strings.NewReader(`{"source":"modelscope","source_id":"org/model"}`))
	rec := httptest.NewRecorder()
	h.createModel(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
	}
	models, err := st.ListModels()
	if err != nil || len(models) != 1 || models[0].Revision != "" || models[0].Name != "org/model" {
		t.Fatalf("models=%+v err=%v", models, err)
	}
}

func TestCreateModelNormalizesModelScopeMainRevision(t *testing.T) {
	st, err := store.Open("file:api_create_model_modelscope_main?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	h := apiHandlers{d: Deps{Store: st, FetchModelMeta: func(_ context.Context, _, _, revision, _ string, _ []string) (*modelmeta.Meta, error) {
		if revision != "master" {
			t.Fatalf("metadata revision=%q, want master", revision)
		}
		return &modelmeta.Meta{}, nil
	}}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/models", strings.NewReader(`{"source":"modelscope","source_id":"org/model","revision":"main"}`))
	rec := httptest.NewRecorder()
	h.createModel(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
	}
	models, err := st.ListModels()
	if err != nil || len(models) != 1 || models[0].Revision != "master" {
		t.Fatalf("models=%+v err=%v", models, err)
	}
}

func TestRegisterLocalModelEndpointIsIdempotent(t *testing.T) {
	st, err := store.Open("file:api_register_local_model_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("node-local", store.HashToken("t"), "local")
	if err != nil {
		t.Fatal(err)
	}
	config := `{"model_type":"qwen2","architectures":["Qwen2ForCausalLM"],"torch_dtype":"float16","max_position_embeddings":4096}`
	h := apiHandlers{d: Deps{
		Store: st,
		CallNode: func(ctx context.Context, uuid, method string, params any) (json.RawMessage, error) {
			switch method {
			case proto.RPCRegisterPath:
				return json.RawMessage(`{"path":"/models/Qwen2","size_bytes":99}`), nil
			case proto.RPCFSReadFile:
				p := params.(map[string]any)
				if strings.HasSuffix(p["path"].(string), "/config.json") {
					return json.RawMessage(`{"content":` + strconv.Quote(config) + `,"size_bytes":10}`), nil
				}
				return json.RawMessage(`{"content":"readme"}`), nil
			default:
				return nil, fmt.Errorf("unexpected rpc %s", method)
			}
		},
	}}
	body := `{"node_uuid":"node-local","path":"/models/Qwen2","name":"Qwen Two"}`
	for i := 0; i < 2; i++ {
		req := httptest.NewRequest(http.MethodPost, "/api/v1/models/local", strings.NewReader(body))
		rec := httptest.NewRecorder()
		h.registerLocalModel(rec, req)
		if rec.Code != http.StatusCreated {
			t.Fatalf("attempt %d status = %d body=%s", i, rec.Code, rec.Body.String())
		}
	}
	models, err := st.ListModels()
	if err != nil {
		t.Fatal(err)
	}
	if len(models) != 1 || models[0].Source != "local" || models[0].Name != "Qwen Two" {
		t.Fatalf("models = %+v", models)
	}
	files, err := st.ModelFilesForNode(node.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(files) != 1 || files[0].State != proto.MFRegistered || files[0].Downloaded != 99 {
		t.Fatalf("model files = %+v", files)
	}
}

// agentFSReadFile answers fs.read_file the way an agent does: an absent file is
// an error, unless the request carries missing_ok and the agent knows the flag.
func agentFSReadFile(files map[string]string, knowsMissingOK bool) func(context.Context, string, string, any) (json.RawMessage, error) {
	return func(ctx context.Context, uuid, method string, params any) (json.RawMessage, error) {
		switch method {
		case proto.RPCRegisterPath:
			return json.RawMessage(`{"path":"/models/gguf","size_bytes":5}`), nil
		case proto.RPCFSReadFile:
			p := params.(map[string]any)
			path := p["path"].(string)
			if content, ok := files[path]; ok {
				return json.RawMessage(`{"path":` + strconv.Quote(path) + `,"content":` + strconv.Quote(content) + `,"size_bytes":` + strconv.Itoa(len(content)) + `}`), nil
			}
			if missingOK, _ := p["missing_ok"].(bool); missingOK && knowsMissingOK {
				return json.RawMessage(`{"path":` + strconv.Quote(path) + `,"missing":true}`), nil
			}
			return nil, fmt.Errorf("stat %s: no such file or directory", path)
		default:
			return nil, fmt.Errorf("unexpected rpc %s", method)
		}
	}
}

// What a model directory must contain is the engine's business, so a directory
// without config.json registers; it only states no metadata.
func TestRegisterLocalModelWithoutConfig(t *testing.T) {
	st, err := store.Open("file:api_register_local_model_without_config?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterNode("node-gguf", store.HashToken("t"), "gguf"); err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st, CallNode: agentFSReadFile(map[string]string{"/models/gguf/README.md": "# gguf"}, true)}}
	rec := httptest.NewRecorder()
	h.registerLocalModel(rec, httptest.NewRequest(http.MethodPost, "/api/v1/models/local", strings.NewReader(`{"node_uuid":"node-gguf","path":"/models/gguf"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	models, err := st.ListModels()
	if err != nil || len(models) != 1 {
		t.Fatalf("models=%+v err=%v", models, err)
	}
	model := models[0]
	if model.Meta != "" || model.Arch != "" || model.WeightBytes != 5 || model.README != "# gguf" ||
		model.Operations != `[]` || model.InputModalities != `["text"]` {
		t.Fatalf("model = %+v", model)
	}
}

// An agent that predates missing_ok answers an absent config.json with an
// error; that error cannot be told from "could not read", so it still refuses.
func TestRegisterLocalModelWithoutConfigOnAnOlderAgent(t *testing.T) {
	st, err := store.Open("file:api_register_local_model_older_agent?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.RegisterNode("node-old", store.HashToken("t"), "old"); err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st, CallNode: agentFSReadFile(nil, false)}}
	rec := httptest.NewRecorder()
	h.registerLocalModel(rec, httptest.NewRequest(http.MethodPost, "/api/v1/models/local", strings.NewReader(`{"node_uuid":"node-old","path":"/models/gguf"}`)))
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "read config.json") {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if models, _ := st.ListModels(); len(models) != 0 {
		t.Fatalf("models = %+v", models)
	}
}

func TestCreateModelUsesBrowserMetadataWithoutServerFetch(t *testing.T) {
	st, err := store.Open("file:api_create_browser_model_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	fetched := false
	logoDir := t.TempDir()
	h := apiHandlers{d: Deps{
		Store:        st,
		ModelLogoDir: logoDir,
		FetchModelMeta: func(context.Context, string, string, string, string, []string) (*modelmeta.Meta, error) {
			fetched = true
			return nil, fmt.Errorf("server fetch should not run")
		},
	}}
	body := `{"source":"huggingface","source_id":"acme/model","revision":"main","readme":"# from browser","config_raw":"{\"model_type\":\"qwen2\",\"architectures\":[\"Qwen2ForCausalLM\"],\"torch_dtype\":\"float16\",\"max_position_embeddings\":4096}","weight_bytes":1234,"logo_data":"data:image/png;base64,iVBORw0KGgoAAAANSUhEUgAAAAEAAAABCAQAAAC1HAwCAAAAC0lEQVR42mNk+A8AAQUBAScY42YAAAAASUVORK5CYII=","browser_metadata":true}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/models", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.createModel(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if fetched {
		t.Fatal("server metadata fetch ran for browser metadata")
	}
	models, err := st.ListModels()
	if err != nil || len(models) != 1 {
		t.Fatalf("models=%+v err=%v", models, err)
	}
	model := models[0]
	if model.README != "# from browser" || model.WeightBytes != 1234 || model.Arch != "Qwen2ForCausalLM" || model.ContextLength != 4096 || model.LogoPath == "" {
		t.Fatalf("browser metadata was not applied: %+v", model)
	}
	if _, err := os.Stat(filepath.Join(logoDir, model.LogoPath)); err != nil {
		t.Fatalf("browser logo was not cached: %v", err)
	}
}

func TestCreateModelClampsBrowserREADME(t *testing.T) {
	st, err := store.Open("file:api_create_browser_readme_limit_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st}}
	readme := strings.Repeat("x", maxModelREADMEBytes+100)
	body := fmt.Sprintf(`{"source":"huggingface","source_id":"acme/large-readme","revision":"main","readme":%s,"browser_metadata":true}`, strconv.Quote(readme))
	req := httptest.NewRequest(http.MethodPost, "/api/v1/models", strings.NewReader(body))
	rec := httptest.NewRecorder()
	h.createModel(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	models, err := st.ListModels()
	if err != nil || len(models) != 1 || len(models[0].README) != maxModelREADMEBytes {
		t.Fatalf("readme length=%d models=%+v err=%v", len(models[0].README), models, err)
	}
}

func TestRefreshHuggingFaceModelUsesBrowserMetadataWithoutServerFetch(t *testing.T) {
	st, err := store.Open("file:api_refresh_browser_model_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "acme/model", Revision: "main", Name: "model"})
	if err != nil {
		t.Fatal(err)
	}
	fetched := false
	h := apiHandlers{d: Deps{
		Store: st,
		FetchModelMeta: func(context.Context, string, string, string, string, []string) (*modelmeta.Meta, error) {
			fetched = true
			return nil, fmt.Errorf("server fetch should not run")
		},
	}}
	body := `{"readme":"# refreshed in browser","config_raw":"{\"model_type\":\"qwen2\",\"architectures\":[\"Qwen2ForCausalLM\"],\"torch_dtype\":\"float16\",\"max_position_embeddings\":8192}","weight_bytes":5678,"browser_metadata":true}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/models/1/metadata/refresh", strings.NewReader(body))
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": fmt.Sprint(model.ID)}))
	rec := httptest.NewRecorder()
	h.refreshModelMetadata(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	if fetched {
		t.Fatal("server metadata fetch ran for browser metadata")
	}
	updated, err := st.GetModel(model.ID)
	if err != nil || updated.README != "# refreshed in browser" || updated.WeightBytes != 5678 || updated.Arch != "Qwen2ForCausalLM" || updated.ContextLength != 8192 {
		t.Fatalf("updated model=%+v err=%v", updated, err)
	}
}

func TestRefreshHuggingFaceModelRejectsEmptyBrowserMetadata(t *testing.T) {
	st, err := store.Open("file:api_refresh_empty_browser_model_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "acme/model", Revision: "main", Name: "model"})
	if err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/models/1/metadata/refresh", strings.NewReader(`{"browser_metadata":true}`))
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": fmt.Sprint(model.ID)}))
	rec := httptest.NewRecorder()
	h.refreshModelMetadata(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"`+ErrorModelMetadataUnavailable+`"`) {
		t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
	}
}

func TestDownloadModelRejectsActivePeerSync(t *testing.T) {
	st, err := store.Open("file:api_download_active_peer_sync?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	model, _ := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "Qwen/Qwen3", Revision: "main", Name: "Qwen3"})
	node, _ := st.RegisterNode("node-1", store.HashToken("t"), "node")
	if _, err := st.UpsertModelFile(&store.ModelFile{
		ModelID: model.ID, NodeID: node.ID, Path: "Qwen3", State: proto.MFSyncing,
		Acquisition: proto.ModelAcquirePeer, TransferID: "transfer-1",
	}); err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/models/1/download", strings.NewReader(`{"node_uuid":"node-1"}`))
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": "1"}))
	rec := httptest.NewRecorder()
	h.downloadModel(rec, req)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), "cancel the active peer sync") {
		t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
	}
	got, _ := st.ModelFileByModelNode(model.ID, node.ID)
	if got.State != proto.MFSyncing || got.TransferID != "transfer-1" {
		t.Fatalf("active peer sync was changed: %+v", got)
	}
}

func TestDownloadModelLeavesARunningDownloadAndRestartsAFailedOne(t *testing.T) {
	st, err := store.Open("file:api_download_running?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	model, _ := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "Qwen/Qwen3", Revision: "main", Name: "Qwen3"})
	node, _ := st.RegisterNode("node-1", store.HashToken("t"), "node")
	row, err := st.UpsertModelFile(&store.ModelFile{
		ModelID: model.ID, NodeID: node.ID, Path: "Qwen3", State: proto.MFDownloading,
		Acquisition: proto.ModelAcquireDownload, TransferID: "download-1",
	})
	if err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st}}
	download := func() {
		t.Helper()
		req := httptest.NewRequest(http.MethodPost, "/api/v1/models/1/download", strings.NewReader(`{"node_uuid":"node-1"}`))
		req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": fmt.Sprint(model.ID)}))
		rec := httptest.NewRecorder()
		h.downloadModel(rec, req)
		if rec.Code != http.StatusAccepted || !strings.Contains(rec.Body.String(), `"status":"downloading"`) {
			t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
		}
	}

	download()
	got, _ := st.ModelFileByModelNode(model.ID, node.ID)
	if got.State != proto.MFDownloading || got.TransferID != "download-1" {
		t.Fatalf("a running download was restarted: %+v", got)
	}

	if err := st.UpdateModelFileStatus(node.ID, proto.ModelFileStatus{
		Key: store.ModelFileKey(row.ID), State: proto.MFFailed, TransferID: "download-1",
		Acquisition: proto.ModelAcquireDownload, Message: "connection reset",
	}); err != nil {
		t.Fatal(err)
	}
	download()
	got, _ = st.ModelFileByModelNode(model.ID, node.ID)
	if got.State != proto.MFPending || got.TransferID == "" || got.TransferID == "download-1" {
		t.Fatalf("a failed download was not prepared as a new transfer: %+v", got)
	}
}

func TestDownloadModelRejectsLocalSource(t *testing.T) {
	st, err := store.Open("file:api_download_local_source?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	model, err := st.UpsertModel(&store.Model{Source: "local", SourceID: "node/model", Name: "local"})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("node-local-download", store.HashToken("t"), "node")
	if err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/models/1/download", strings.NewReader(`{"node_uuid":"node-local-download"}`))
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": fmt.Sprint(model.ID)}))
	rec := httptest.NewRecorder()
	h.downloadModel(rec, req)
	if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), "local models cannot be downloaded") {
		t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
	}
	if _, err := st.ModelFileByModelNode(model.ID, node.ID); err == nil {
		t.Fatal("local download guard created a model file")
	}
}

func TestDownloadModelRejectsWhenTheStorageIsDisabled(t *testing.T) {
	st, err := store.Open("file:api_download_storage_disabled?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	storages, err := st.ListStorages()
	if err != nil || len(storages) != 1 {
		t.Fatalf("storages = %+v err=%v", storages, err)
	}
	disabled := false
	if _, err := st.UpdateStorage(storages[0].ID, store.StoragePatch{Enabled: &disabled}); err != nil {
		t.Fatal(err)
	}
	model, _ := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "Qwen/Qwen3", Revision: "main", Name: "Qwen3"})
	if _, err := st.RegisterNode("node-1", store.HashToken("t"), "node"); err != nil {
		t.Fatal(err)
	}
	h := apiHandlers{d: Deps{Store: st}}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/models/1/download", strings.NewReader(`{"node_uuid":"node-1"}`))
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": fmt.Sprint(model.ID)}))
	rec := httptest.NewRecorder()
	h.downloadModel(rec, req)
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"code":"`+ErrorStorageDisabled+`"`) {
		t.Fatalf("status/body = %d %s", rec.Code, rec.Body.String())
	}
	if _, err := st.ModelFileByModelNode(model.ID, 1); err == nil {
		t.Fatal("disabled storage created a download row")
	}
}

// Registering a model outside the catalog seeds its API contract once from
// metadata. An unreadable source leaves operations unknown. A metadata refresh afterwards updates the repository facts and
// leaves the contract alone, empty or not: the contract is not a fact of the
// repository, and a template's weights get theirs from the template.
func TestModelContractIsSeededAtRegistrationAndNotByRefresh(t *testing.T) {
	st, err := store.Open("file:api_model_contract_seed_refresh?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	reachable := false
	h := apiHandlers{d: Deps{Store: st, FetchModelMeta: func(context.Context, string, string, string, string, []string) (*modelmeta.Meta, error) {
		if !reachable {
			return nil, fmt.Errorf("source unreachable")
		}
		return &modelmeta.Meta{
			Config:    &modelconfig.Config{ModelType: "qwen2_vl", Architectures: []string{"Qwen2VLForConditionalGeneration"}, VisionConfig: &modelconfig.TowerConfig{}},
			ConfigRaw: `{"model_type":"qwen2_vl","architectures":["Qwen2VLForConditionalGeneration"],"vision_config":{"depth":32}}`,
			Arch:      "Qwen2VLForConditionalGeneration", WeightBytes: 7,
		}, nil
	}}}
	rec := httptest.NewRecorder()
	h.createModel(rec, httptest.NewRequest(http.MethodPost, "/api/v1/models", strings.NewReader(`{"source":"huggingface","source_id":"acme/offline","revision":"main"}`)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	models, err := st.ListModels()
	if err != nil || len(models) != 1 || models[0].Operations != `[]` || models[0].InputModalities != `["text"]` {
		t.Fatalf("registered without metadata: %+v err=%v", models, err)
	}

	template, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "acme/template-weights", Revision: "master", Name: "acme/template-weights"})
	if err != nil {
		t.Fatal(err)
	}
	reachable = true
	for _, model := range []*store.Model{&models[0], template} {
		refresh := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, fmt.Sprintf("/api/v1/models/%d/metadata/refresh", model.ID), nil)
		routeContext := chi.NewRouteContext()
		routeContext.URLParams.Add("id", fmt.Sprint(model.ID))
		h.refreshModelMetadata(refresh, request.WithContext(context.WithValue(request.Context(), chi.RouteCtxKey, routeContext)))
		if refresh.Code != http.StatusOK {
			t.Fatalf("refresh %d: status=%d body=%s", model.ID, refresh.Code, refresh.Body.String())
		}
		after, err := st.GetModel(model.ID)
		if err != nil || after.Arch != "Qwen2VLForConditionalGeneration" || after.Operations != model.Operations || after.InputModalities != model.InputModalities {
			t.Fatalf("refresh changed the contract of %d: before ops=%q mods=%q, after %+v err=%v", model.ID, model.Operations, model.InputModalities, after, err)
		}
	}
}
