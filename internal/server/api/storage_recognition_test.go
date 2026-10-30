package api

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/modelscan"
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
	"gpuworks.ai/gpuworks/pkg/model/modelmetadata"
)

func TestStorageImportReparsesMetadataAndKeepsEntryDirectoryIdentity(t *testing.T) {
	root := t.TempDir()
	write := func(name, body string) {
		t.Helper()
		p := filepath.Join(root, name)
		if err := os.MkdirAll(filepath.Dir(p), 0755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(p, []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	write("org/model/README.md", "---\npipeline_tag: text-ranking\n---\n")
	for _, sub := range []string{"a", "b"} {
		write("org/model/"+sub+"/config.json", `{"architectures":["Qwen3ForCausalLM"]}`)
		write("org/model/"+sub+"/model.safetensors", "weights")
	}
	write("org/pipeline/model_index.json", `{"_class_name":"ExamplePipeline","encoder":["transformers","Encoder"]}`)
	write("org/pipeline/encoder/config.json", `{"architectures":["ExampleModel"]}`)
	write("org/pipeline/encoder/model.safetensors", "weights")
	write("org/dual/modular_model_index.json", `{"_class_name":"ExampleModularPipeline","backbone":["transformers","ExampleModel"]}`)
	write("org/dual/backbone/config.json", `{"architectures":["ExampleModel"]}`)
	write("org/dual/backbone/model.safetensors", "weights")
	write("org/dual/model_index.json", `{"_class_name":"ExamplePipeline","vision_encoder":["transformers","Qwen3VLVisionModel"]}`)
	write("org/dual/vision_encoder/config.json", `{"architectures":["Qwen3VLVisionModel"],"model_type":"qwen3_vl"}`)
	write("org/dual/vision_encoder/model.safetensors", "weights")
	st, err := store.Open(filepath.Join(t.TempDir(), "db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	node, err := st.RegisterNode("node", store.HashToken("token"), "node")
	if err != nil {
		t.Fatal(err)
	}
	shared, err := st.CreateStorage(&store.Storage{Name: "shared", Type: proto.StorageNFS, Enabled: true, AllNodes: true, Path: root, Export: "example:/models"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	whole, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/model", Revision: "main", Name: "whole", Operations: `[{"type":"openai.chat"}]`})
	if err != nil {
		t.Fatal(err)
	}
	remoteSub, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/model", Revision: "main", ArtifactSubfolder: "a", Name: "remote subtree"})
	if err != nil {
		t.Fatal(err)
	}
	failRPC := false
	h := apiHandlers{d: Deps{Store: st, CallNode: func(ctx context.Context, uuid, method string, params any) (json.RawMessage, error) {
		if failRPC {
			return nil, errors.New("storage unavailable")
		}
		if uuid != node.UUID || method != proto.RPCRegisterPath {
			t.Fatalf("unexpected RPC: %s %s", uuid, method)
		}
		p := params.(map[string]any)
		if p["inspect_metadata"] != true || p["storage_id"] != shared.ID {
			t.Fatalf("metadata parsing not scoped to storage: %v", p)
		}
		dir := p["path"].(string)
		report, err := modelscan.ParseMetadataWithin(ctx, root, dir)
		if err != nil {
			return nil, err
		}
		size, err := modelscan.ValidateModelDir(dir)
		if err != nil {
			return nil, err
		}
		return json.Marshal(registerPathResponse{Path: dir, SizeBytes: size, Metadata: &report})
	}}}
	page := proto.StorageScanResult{Candidates: []proto.StorageScanCandidate{{RelPath: "org/model/a", Source: "local", SourceID: "org/model/a", Format: "transformers", Tasks: []string{"text-ranking"}}}, Complete: true}
	annotated := h.annotateScan(shared, node, page)
	if annotated.Candidates[0].BindModelID != 0 || annotated.Candidates[0].Subfolder != "" {
		t.Fatalf("subfolder bound to repository root: %+v", annotated)
	}
	var ids []uint
	for _, sub := range []string{"a", "b"} {
		item := dto.StorageImportItem{RelPath: "org/model/" + sub, Source: "huggingface", SourceID: "org/model", Revision: "main", Subfolder: sub, SizeBytes: 999999}
		result, err := h.importOne(context.Background(), shared, node, root, item)
		if err != nil || !result.Created || result.ModelID == whole.ID {
			t.Fatalf("import=%+v %v", result, err)
		}
		ids = append(ids, result.ModelID)
		m, err := st.GetModel(result.ModelID)
		if err != nil || m.Source != "local" || m.SourceID != item.RelPath || m.ArtifactSubfolder != "" || !strings.Contains(m.Operations, "openai.rerank") || m.WeightBytes == 999999 {
			t.Fatalf("metadata not persisted correctly: %+v %v", m, err)
		}
		f, err := st.ModelFileAt(store.FileRef{ModelID: m.ID, StorageID: shared.ID})
		if err != nil || f.Path != item.RelPath || f.SizeBytes != m.WeightBytes {
			t.Fatalf("file binding=%+v %v", f, err)
		}
		if err := st.DB().Model(m).Update("operations", `[{"type":"openai.audio.speech"}]`).Error; err != nil {
			t.Fatal(err)
		}
		again, err := h.importOne(context.Background(), shared, node, root, item)
		preserved, _ := st.GetModel(m.ID)
		if err != nil || again.Created || !strings.Contains(preserved.Operations, "openai.audio.speech") {
			t.Fatalf("reimport changed contract: %+v %v", preserved, err)
		}
	}
	if ids[0] == ids[1] {
		t.Fatal("different subfolders share identity")
	}
	annotated = h.annotateScan(shared, node, page)
	if annotated.Candidates[0].BindModelID != ids[0] {
		t.Fatalf("wrong subfolder binding: %+v", annotated)
	}
	if h.bindingModel("huggingface", "org/model", "other-revision", "a") != nil {
		t.Fatal("binding crossed revisions")
	}
	if _, err := h.importOne(context.Background(), shared, node, root, dto.StorageImportItem{RelPath: "org/model/a", ModelID: remoteSub.ID, Subfolder: "a"}); err == nil {
		t.Fatal("standalone directory bound to a repository-preserving artifact")
	}
	parent, err := h.importOne(context.Background(), shared, node, root, dto.StorageImportItem{RelPath: "org/dual", Source: "local", SourceID: "org/dual"})
	if err != nil || !parent.Created {
		t.Fatalf("dual-manifest parent must remain importable: %+v %v", parent, err)
	}
	before, _ := st.ListModels()
	for _, item := range []dto.StorageImportItem{
		{RelPath: "org/pipeline/encoder", Source: "local", SourceID: "component"},
		{RelPath: "org/dual/vision_encoder", Source: "local", SourceID: "vision-component"},
		{RelPath: "../escape", Source: "local", SourceID: "outside"},
		{RelPath: "gone", Source: "local", SourceID: "gone"},
	} {
		if _, err := h.importOne(context.Background(), shared, node, root, item); err == nil {
			t.Fatalf("invalid import accepted: %+v", item)
		}
	}
	failRPC = true
	if _, err := h.importOne(context.Background(), shared, node, root, dto.StorageImportItem{RelPath: "org/model/a", ModelID: ids[0]}); err == nil {
		t.Fatal("RPC failure accepted")
	}
	after, _ := st.ListModels()
	if len(after) != len(before) {
		t.Fatal("failed metadata parsing wrote a model")
	}
}

func TestMetadataReportSeedsSingleModelPreview(t *testing.T) {
	st, err := store.Open(filepath.Join(t.TempDir(), "db"), bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { st.Close() })
	node, err := st.RegisterNode("node", store.HashToken("t"), "node")
	if err != nil {
		t.Fatal(err)
	}
	report := modelmetadata.Report{
		Entries:   []modelmetadata.Entry{{Path: ".", Format: "transformers", Files: []string{"model.safetensors"}}},
		ConfigRaw: `{"architectures":["BertModel"]}`, Tasks: []string{"feature-extraction"},
	}
	h := apiHandlers{d: Deps{Store: st, CallNode: func(_ context.Context, _, method string, params any) (json.RawMessage, error) {
		if method != proto.RPCRegisterPath || params.(map[string]any)["inspect_metadata"] != true {
			t.Fatalf("unexpected metadata fetch: %s", method)
		}
		return json.Marshal(registerPathResponse{Path: "/model", SizeBytes: 123, Metadata: &report})
	}}}
	preview, err := h.buildLocalModelPreview(context.Background(), node.UUID, "/model")
	if err != nil || len(preview.Operations) != 1 || preview.Operations[0].Type != "openai.embeddings" {
		t.Fatalf("preview=%+v err=%v", preview, err)
	}
	report.Tasks = nil
	preview, err = h.buildLocalModelPreview(context.Background(), node.UUID, "/model")
	if err != nil || len(preview.Operations) != 0 {
		t.Fatalf("unknown defaulted to chat: %+v %v", preview, err)
	}
}
