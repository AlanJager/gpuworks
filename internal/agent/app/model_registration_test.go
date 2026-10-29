package app

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestRegisterPathRejectsParentAndInvalidMetadata(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
	}{
		{"parent", map[string]string{"first/config.json": `{"model_type":"qwen3"}`, "first/model.safetensors": "weights", "second/config.json": `{"model_type":"qwen3"}`, "second/model.safetensors": "other weights"}},
		{"metadata-only", map[string]string{"config.json": `{"model_type":"qwen3"}`}},
		{"incomplete-shards", map[string]string{"config.json": `{"model_type":"qwen3"}`, "model.safetensors.index.json": `{"weight_map":{"weight":"missing.safetensors"}}`}},
		{"non-model", map[string]string{"notes.txt": "notes"}},
		{"parent-with-config", map[string]string{"config.json": `{"model_type":"qwen3"}`, "child/config.json": `{"model_type":"qwen3"}`, "child/model.safetensors": "weights"}},
		{"nested-config", map[string]string{"child/config.json": `{}`, "model.safetensors": "weights"}},
		{"malformed-config", map[string]string{"config.json": `{`, "model.safetensors": "weights"}},
		{"null-config", map[string]string{"config.json": `null`, "model.safetensors": "weights"}},
		{"array-config", map[string]string{"config.json": `[]`, "model.safetensors": "weights"}},
		{"trailing-config", map[string]string{"config.json": `{} {}`, "model.safetensors": "weights"}},
		{"oversized-config", map[string]string{"config.json": `{"padding":"` + strings.Repeat("x", 3*1024*1024) + `"}`, "model.safetensors": "weights"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, body := range tc.files {
				path := filepath.Join(dir, name)
				if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(body), 0644); err != nil {
					t.Fatal(err)
				}
			}
			n := &NodeRuntime{}
			if result, err := n.OnRPC(context.Background(), proto.RPCRegisterPath, mustRaw(t, map[string]string{"path": dir})); err == nil {
				t.Fatalf("invalid registration succeeded: %+v", result)
			}
		})
	}
}

func TestRegisterPathRetainsSupportedLayoutsAndExactSize(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
	}{
		{"huggingface", map[string]string{"config.json": `{"model_type":"qwen3"}`, "model.safetensors": "weights", "README.md": "readme"}},
		{"native-mistral", map[string]string{"params.json": `{"dim":4096}`, "tekken.json": `{}`, "consolidated.safetensors": "weights"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			var want int64
			for name, body := range tc.files {
				path := filepath.Join(dir, name)
				if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(path, []byte(body), 0644); err != nil {
					t.Fatal(err)
				}
				want += int64(len(body))
			}
			n := &NodeRuntime{}
			result, err := n.OnRPC(context.Background(), proto.RPCRegisterPath, mustRaw(t, map[string]string{"path": dir}))
			if err != nil {
				t.Fatal(err)
			}
			got := result.(registerPathResult)
			if got.Path != dir || got.SizeBytes != want {
				t.Fatalf("result=%+v want bytes=%d", got, want)
			}
		})
	}
}

// A real GGUF header carries architecture metadata without config.json.
func registrationGGUF() []byte {
	var b bytes.Buffer
	b.WriteString("GGUF")
	binary.Write(&b, binary.LittleEndian, uint32(3))
	binary.Write(&b, binary.LittleEndian, uint64(1))
	binary.Write(&b, binary.LittleEndian, uint64(1))
	key := "general.architecture"
	binary.Write(&b, binary.LittleEndian, uint64(len(key)))
	b.WriteString(key)
	binary.Write(&b, binary.LittleEndian, uint32(8))
	binary.Write(&b, binary.LittleEndian, uint64(len("llama")))
	b.WriteString("llama")
	return b.Bytes()
}
