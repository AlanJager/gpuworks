package deploy

import (
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/internal/testenv"
)

// These cases replay the 2026-09-17 mirror1 incident verbatim: a node whose
// catalog held two custom entries that tied under correct scoring — one fully
// declared, one missing framework_version — next to the official series. The
// create path once omitted the node driver from its resolution input, so the
// preview and the create picked different images and every submission of the
// preview's answer was rejected as a stale candidate. The requests below are
// the shapes recorded in the server's access log that afternoon.
const (
	incidentOfficialRef = "gpustack/runner:cuda13.0-vllm0.25.1"
	incidentDeclaredRef = "gpustack/runner:cuda13.0-vllm0.27.1"
	incidentQuayRef     = "quay.io/gpustack/runner:cuda13.0-vllm0.27.1"
	incidentSWRRef      = "swr.cn-south-1.myhuaweicloud.com/gpustack/runner:cuda13.0-vllm0.27.1"
	incidentUndeclared  = "vllm/vllm-openai:v0.29.0"
	incidentUndeclaredD = "docker.io/vllm/vllm-openai:v0.29.0"
)

// seedIncidentCatalog writes the catalog rows as they existed on mirror1: an
// official multi-arch series, plus the "vLLM Upstream (kvtest)" custom series
// imported during kv-cache testing whose v0.29.0 entry carries no
// framework_version — the import gate rejects that shape today, so the row is
// written straight into the store, which is also how the legacy row got there
// before the gate existed. One deviation: the kvtest rows were amd64 on the
// real node; here they are multi because the harness reports the test host's
// architecture. Arch specificity neither filters nor scores since the
// points-system removal, so every property under test is unchanged.
func seedIncidentCatalog(t *testing.T, st *store.Store) {
	t.Helper()
	// The catalog is exactly mirror1's: the embedded system images, which
	// would outrank the incident rows, are not part of it.
	clearSystemImages(t, st)
	official, err := st.UpsertImageSeries(&store.ImageSeries{Name: "vLLM", Engine: "vllm", Purpose: store.ImagePurposeInference, Framework: "cuda", Source: "official", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertImage(&store.Image{
		SeriesID: official.ID, Name: "vLLM", Engine: "vllm", EngineVersion: "0.25.1",
		Framework: "cuda", FrameworkVersion: "13.0", Arch: "multi",
		ImageRef: incidentOfficialRef, Source: "official", Enabled: true,
	}); err != nil {
		t.Fatal(err)
	}
	kvtest, err := st.UpsertImageSeries(&store.ImageSeries{Name: "vLLM Upstream (kvtest)", Engine: "vllm", Purpose: store.ImagePurposeInference, Framework: "cuda", Source: "custom", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertImage(&store.Image{
		SeriesID: kvtest.ID, Name: "vLLM Upstream", Engine: "vllm", EngineVersion: "0.27.1",
		Framework: "cuda", FrameworkVersion: "13.0", Arch: "multi",
		ImageRef: incidentDeclaredRef, Source: "custom", Enabled: true,
		RegistrySources: `["` + incidentQuayRef + `","` + incidentSWRRef + `"]`,
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertImage(&store.Image{
		SeriesID: kvtest.ID, Name: "vLLM Upstream", Engine: "vllm", EngineVersion: "0.29.0",
		Framework: "cuda", Arch: "multi",
		ImageRef: incidentUndeclared, Source: "custom", Enabled: true,
		RegistrySources: `["` + incidentUndeclaredD + `"]`,
	}); err != nil {
		t.Fatal(err)
	}
}

// incidentEnv brings up the host-3080 shape: one CUDA node (the fake prober
// reports a driver well past the CUDA 13 minimum, like the real 595.99.02)
// with Qwen3-0.6B already complete on disk, so "local_present" is the honest
// model choice the form made that day.
func incidentEnv(t *testing.T) (*testenv.Env, *store.Model, string) {
	t.Helper()
	env := testenv.New(t)
	env.AddNode("host-3080",
		testenv.WithGPUs(1, testenv.GPUSpec{Model: "NVIDIA GeForce RTX 3080 Ti", VRAMMB: 12288, Vendor: "nvidia", ArchID: "sm_86", ComputeCapability: "8.6"}),
		testenv.WithAccelerator("nvidia", "cuda", "13.2"),
		testenv.WithGPUDriver("595.99.02"),
	)
	env.WaitNodeOnline(t, "host-3080")
	seedIncidentCatalog(t, env.Store())
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}
	modelDir := filepath.Join(t.TempDir(), "Qwen3-0.6B")
	if err := os.MkdirAll(modelDir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "config.json"), []byte(`{"model_type":"qwen3"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(modelDir, "model.safetensors"), []byte("weights"), 0o644); err != nil {
		t.Fatal(err)
	}
	// A legacy row may still bind the model to an image; the binding has no
	// reader anymore and must not influence resolution either way.
	bound, err := env.Store().UpsertImage(&store.Image{SeriesID: 0, Name: "bound sglang", Engine: "sglang", Framework: "cuda", FrameworkVersion: "12.4", Arch: "amd64", ImageRef: "bound/sglang:v1", Source: "custom", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	model, err := env.Store().UpsertModel(&store.Model{
		Source: "modelscope", SourceID: "Qwen/Qwen3-0.6B", Revision: "09b42cad", Name: "Qwen3-0.6B",
		Arch: "Qwen3ForCausalLM", WeightBytes: 1 << 30, ImageID: bound.ID,
		Meta: `{"model_type":"qwen3","num_hidden_layers":28,"hidden_size":1024,"num_attention_heads":16,"num_key_value_heads":8,"max_position_embeddings":32768}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := env.Store().RegisterModelFile(store.FileRef{ModelID: model.ID, NodeID: nodes[0].ID}, modelDir, 1<<30); err != nil {
		t.Fatal(err)
	}
	return env, model, nodes[0].UUID
}

func postDeployment(t *testing.T, env *testenv.Env, body map[string]any) (int, map[string]any) {
	t.Helper()
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	response, err := env.API(t).PostJSON("/api/v1/deployments", encoded)
	if err != nil {
		t.Fatal(err)
	}
	defer response.Body.Close()
	raw, _ := io.ReadAll(response.Body)
	out := map[string]any{}
	_ = json.Unmarshal(raw, &out)
	return response.StatusCode, out
}

// The form's flow must close: what the preview resolves and shows is what the
// create deploys. On mirror1 the same two requests returned 400 forever.
func TestPreviewAnswerDeploysEndToEnd(t *testing.T) {
	env, model, nodeUUID := incidentEnv(t)

	preview := evaluateAcquisition(t, env, map[string]any{
		"engine": "vllm", "model_id": model.ID, "node_uuid": nodeUUID,
		"scenario": "balanced", "served_name": "Qwen/Qwen3-0.6B",
		"quick_params": map[string]any{"gpu_memory_utilization": 0.9},
	})
	// The declared 13.0 entry outranks the version-less one under a driver
	// that satisfies CUDA 13, and the engine-version ordering decides between
	// the two 13.0 entries.
	if preview.Acquisition.Image.Image != incidentDeclaredRef {
		t.Fatalf("preview resolved %q, want %q", preview.Acquisition.Image.Image, incidentDeclaredRef)
	}
	// The registry sources the catalog declares are among the copies the
	// named node is forecast to pull from.
	foundQuay := false
	for _, forecast := range preview.Acquisition.Nodes {
		for _, ref := range forecast.ImagePullRefs {
			if ref == incidentQuayRef {
				foundQuay = true
			}
		}
	}
	if !foundQuay {
		t.Fatalf("declared quay source not among the pull candidates: %+v", preview.Acquisition.Nodes)
	}

	status, created := postDeployment(t, env, map[string]any{
		"engine": "vllm", "model_id": model.ID, "node_uuid": nodeUUID, "name": "qwen3-0-6b",
		"scenario": "balanced", "served_name": "Qwen/Qwen3-0.6B", "sleepable": false,
		"quick_params": map[string]any{"gpu_memory_utilization": 0.9},
	})
	if status != http.StatusCreated {
		t.Fatalf("create status = %d body=%v", status, created)
	}
	if got := created["image"]; got != incidentDeclaredRef {
		t.Fatalf("deployed image = %v, want %q", got, incidentDeclaredRef)
	}
	// Where the image comes from is not the deployment's: nothing about it is
	// recorded.
	snapshot, _ := created["catalog_selection_snapshot"].(map[string]any)
	if _, recorded := snapshot["acquisition"]; recorded {
		t.Fatalf("snapshot records an acquisition: %+v", snapshot)
	}
	// A local copy must not have been turned into a download.
	nodes, err := env.Store().ListNodes()
	if err != nil || len(nodes) != 1 {
		t.Fatalf("nodes=%+v err=%v", nodes, err)
	}
	file, err := env.Store().ModelFileByModelNode(model.ID, nodes[0].ID)
	if err != nil {
		t.Fatal(err)
	}
	if file.Acquisition == proto.ModelAcquireDownload {
		t.Fatalf("local_present scheduled a download: %+v", file)
	}
}

// A client that still sends the retired acquisition fields (an image source
// choice, a model acquisition kind) is not refused: the fields are not part of
// the request any more and say nothing about where nodes get the image.
func TestRetiredAcquisitionFieldsAreIgnored(t *testing.T) {
	env, model, nodeUUID := incidentEnv(t)
	status, created := postDeployment(t, env, map[string]any{
		"engine": "vllm", "model_id": model.ID, "node_uuid": nodeUUID, "name": "stale-plan",
		"model_source": map[string]any{"kind": "local_present"},
		"image_pull": map[string]any{
			"image": incidentUndeclared, "kind": "public",
			"provider": incidentUndeclaredD, "network": map[string]any{"mode": "direct"},
		},
	})
	if status != http.StatusCreated {
		t.Fatalf("status=%d body=%v", status, created)
	}
	if got := created["image"]; got != incidentDeclaredRef {
		t.Fatalf("deployed image = %v, want the resolved %q", got, incidentDeclaredRef)
	}
	snapshot, _ := created["catalog_selection_snapshot"].(map[string]any)
	if _, recorded := snapshot["acquisition"]; recorded {
		t.Fatalf("snapshot records an acquisition: %+v", snapshot)
	}
}

// clearSystemImages removes the embedded system catalog so a test can state
// the whole catalog it runs against.
func clearSystemImages(t *testing.T, st *store.Store) {
	t.Helper()
	for _, statement := range []string{
		"DELETE FROM image_nodes WHERE image_version_id IN (SELECT id FROM image_versions WHERE source = 'official')",
		"DELETE FROM image_versions WHERE source = 'official'",
		"DELETE FROM image_series WHERE source = 'official'",
	} {
		if err := st.DB().Exec(statement).Error; err != nil {
			t.Fatal(err)
		}
	}
}
