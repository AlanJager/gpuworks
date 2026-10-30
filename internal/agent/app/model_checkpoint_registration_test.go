package app

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/modelscan"
	"gpuworks.ai/gpuworks/internal/proto"
)

func TestRegisterQuantizedCheckpointAndScan(t *testing.T) {
	root := t.TempDir()
	dir := filepath.Join(root, "Qwen3.8-27B-w8a8")
	if err := os.Mkdir(dir, 0755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{"config.json": `{"architectures":["ExampleForCausalLM"]}`, "quant_model_description.json": `{}`}
	weightMap := map[string]string{}
	for i := 1; i <= 10; i++ {
		name := fmt.Sprintf("quant_model_weights-%05d-of-00010.safetensors", i)
		weightMap[fmt.Sprintf("tensor.%d", i)] = name
		files[name] = "fixture weights; no tensor loading in registration"
	}
	index, err := json.Marshal(map[string]any{"weight_map": weightMap})
	if err != nil {
		t.Fatal(err)
	}
	files["quant_model_weights.safetensors.index.json"] = string(index)
	for name, body := range files {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0644); err != nil {
			t.Fatal(err)
		}
	}
	n := &NodeRuntime{}
	result, err := n.OnRPC(context.Background(), proto.RPCRegisterPath, mustRaw(t, map[string]any{"path": dir, "inspect_metadata": true}))
	if err != nil {
		t.Fatalf("reported model layout rejected: %v", err)
	}
	got := result.(registerPathResult)
	if got.Metadata == nil || len(got.Metadata.Entries) != 1 || len(got.Metadata.Entries[0].Files) != 10 || got.SizeBytes <= 0 {
		t.Fatalf("registration=%+v", got)
	}
	scan, err := modelscan.Scan(context.Background(), root, modelscan.Options{})
	if err != nil || len(scan.Candidates) != 1 || scan.Candidates[0].Incomplete {
		t.Fatalf("scan=%+v err=%v", scan, err)
	}
	missing := "quant_model_weights-00010-of-00010.safetensors"
	if err := os.Remove(filepath.Join(dir, missing)); err != nil {
		t.Fatal(err)
	}
	_, err = n.OnRPC(context.Background(), proto.RPCRegisterPath, mustRaw(t, map[string]any{"path": dir}))
	if err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("missing shard registration=%v", err)
	}
}

func TestRegisterCheckpointKeepsMetadataFailure(t *testing.T) {
	for _, tc := range []struct{ name, index, body string }{
		{"standard", "model.safetensors.index.json", `{"weight_map":`},
		{"custom", "quant_model_weights.safetensors.index.json", `{"weight_map":`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, body := range map[string]string{"config.json": `{"model_type":"example"}`, tc.index: tc.body} {
				if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0644); err != nil {
					t.Fatal(err)
				}
			}
			_, err := (&NodeRuntime{}).OnRPC(context.Background(), proto.RPCRegisterPath, mustRaw(t, map[string]string{"path": dir}))
			if err == nil || !strings.Contains(err.Error(), tc.index) || !strings.Contains(err.Error(), "EOF") || strings.Contains(err.Error(), "rather than its parent") {
				t.Fatalf("parse failure hidden: %v", err)
			}
		})
	}
}

func TestRegisterModelScopeQwenQuantizedMetadata(t *testing.T) {
	dir := t.TempDir()
	for _, name := range []string{"config.json", "configuration.json", "quant_model_weights.safetensors.index.json"} {
		body, err := os.ReadFile(filepath.Join("testdata", "modelscope_qwen3_8_w8a8", name))
		if err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(filepath.Join(dir, name), body, 0644); err != nil {
			t.Fatal(err)
		}
	}
	for i := 1; i <= 10; i++ {
		name := fmt.Sprintf("quant_model_weights-%05d-of-00010.safetensors", i)
		if err := os.WriteFile(filepath.Join(dir, name), []byte("placeholder weight payload"), 0644); err != nil {
			t.Fatal(err)
		}
	}
	n := &NodeRuntime{}
	raw := mustRaw(t, map[string]any{"path": dir, "inspect_metadata": true})
	result, err := n.OnRPC(context.Background(), proto.RPCRegisterPath, raw)
	if err != nil {
		t.Fatalf("published ModelScope metadata rejected: %v", err)
	}
	got := result.(registerPathResult)
	if got.Metadata == nil || len(got.Metadata.Entries) != 1 {
		t.Fatalf("metadata=%+v", got.Metadata)
	}
	entry := got.Metadata.Entries[0]
	if entry.Format != "transformers" || entry.Incomplete || len(entry.Files) != 10 || !slices.Equal(entry.Tasks, []string{"image-text-to-text"}) {
		t.Fatalf("entry=%+v", entry)
	}
	t.Logf("published metadata: format=%s shards=%d incomplete=%v tasks=%v", entry.Format, len(entry.Files), entry.Incomplete, entry.Tasks)
	missing := "quant_model_weights-00010-of-00010.safetensors"
	if err := os.Remove(filepath.Join(dir, missing)); err != nil {
		t.Fatal(err)
	}
	_, err = n.OnRPC(context.Background(), proto.RPCRegisterPath, raw)
	if err == nil || !strings.Contains(err.Error(), missing) {
		t.Fatalf("published index missing shard not rejected: %v", err)
	}
	t.Logf("missing shard correctly rejected: %v", err)
}
