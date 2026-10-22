package app

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestRegisterPathMetadataWireAndErrors(t *testing.T) {
	dir := t.TempDir()
	writeModel(t, dir, "weights")
	readme := "---\npipeline_tag: text-ranking\n---\n"
	if err := os.WriteFile(filepath.Join(dir, "README.md"), []byte(readme), 0644); err != nil {
		t.Fatal(err)
	}
	n := &NodeRuntime{}
	raw := mustRaw(t, map[string]any{"path": dir, "inspect_metadata": true})
	result, err := n.OnRPC(context.Background(), proto.RPCRegisterPath, raw)
	if err != nil {
		t.Fatal(err)
	}
	body, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	var wire map[string]json.RawMessage
	if err := json.Unmarshal(body, &wire); err != nil {
		t.Fatal(err)
	}
	if len(wire) != 3 || string(wire["path"]) != string(mustRaw(t, dir)) || len(wire["size_bytes"]) == 0 {
		t.Fatalf("metadata envelope=%s", body)
	}
	var report map[string]any
	if err := json.Unmarshal(wire["metadata"], &report); err != nil {
		t.Fatal(err)
	}
	var want map[string]any
	const expected = `{"entries":[{"path":".","format":"transformers","files":["model.safetensors"],"tasks":["text-ranking"]}],"tasks":["text-ranking"],"evidence":[{"source":"README.md","field":"pipeline_tag","value":"text-ranking"},{"source":"config.json","field":"architectures","value":"text-generation"}],"repository":true,"config_raw":"{\"architectures\":[\"Qwen3ForCausalLM\"]}","readme":"---\npipeline_tag: text-ranking\n---\n"}`
	if err := json.Unmarshal([]byte(expected), &want); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(report, want) {
		t.Fatalf("metadata wire=%s want=%s", wire["metadata"], expected)
	}
	// The optional extension leaves the legacy result unchanged.
	legacy, err := n.OnRPC(context.Background(), proto.RPCRegisterPath, mustRaw(t, map[string]string{"path": dir}))
	if err != nil {
		t.Fatal(err)
	}
	b, _ := json.Marshal(legacy)
	var old map[string]any
	json.Unmarshal(b, &old)
	if len(old) != 2 || old["metadata"] != nil {
		t.Fatalf("legacy response=%s", b)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := n.OnRPC(ctx, proto.RPCRegisterPath, raw); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancellation=%v", err)
	}
	if _, err := n.OnRPC(context.Background(), proto.RPCRegisterPath, mustRaw(t, map[string]any{"path": filepath.Join(dir, "missing"), "inspect_metadata": true})); err == nil {
		t.Fatal("missing directory accepted")
	}
	if err := os.WriteFile(filepath.Join(dir, "unfinished.part"), []byte("part"), 0644); err != nil {
		t.Fatal(err)
	}
	if _, err := n.OnRPC(context.Background(), proto.RPCRegisterPath, raw); err == nil {
		t.Fatal("unfinished directory accepted")
	}
}

func TestStorageMetadataReconstructsComponentOwnership(t *testing.T) {
	r := newStorageTestRuntime(t)
	root := t.TempDir()
	repo := filepath.Join(root, "pipeline")
	writeModel(t, filepath.Join(repo, "encoder"), "weights")
	if err := os.WriteFile(filepath.Join(repo, "model_index.json"), []byte(`{"_class_name":"ExamplePipeline","encoder":["transformers","Encoder"]}`), 0644); err != nil {
		t.Fatal(err)
	}
	r.applyStorages([]proto.StorageTask{{ID: 2, Name: "models", Type: proto.StorageSharedDir, Path: root}})
	<-r.storage.trigger
	r.checkStorages(context.Background(), true)
	raw := mustRaw(t, map[string]any{"path": filepath.Join(repo, "encoder"), "storage_id": 2, "inspect_metadata": true})
	result, err := r.OnRPC(context.Background(), proto.RPCRegisterPath, raw)
	if err != nil {
		t.Fatal(err)
	}
	report := result.(registerPathResult).Metadata
	if report == nil || !report.Component || len(report.Entries) != 0 {
		t.Fatalf("component metadata=%+v", report)
	}
	outside := t.TempDir()
	writeModel(t, outside, "weights")
	if _, err := r.OnRPC(context.Background(), proto.RPCRegisterPath, mustRaw(t, map[string]any{"path": outside, "storage_id": 2, "inspect_metadata": true})); err == nil {
		t.Fatal("storage metadata parsing escaped mount")
	}
}
