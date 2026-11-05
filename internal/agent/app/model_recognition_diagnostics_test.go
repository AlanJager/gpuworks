package app

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestRegisterPathPreservesRecognitionReasons(t *testing.T) {
	for _, tc := range []struct{ name, config, cause string }{
		{"identity", `{}`, "neither model_type nor architectures"},
		{"weights", `{"model_type":"example"}`, "no supported checkpoint weights"},
		{"json", `{`, "unexpected end of JSON input"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			if err := os.WriteFile(filepath.Join(dir, "config.json"), []byte(tc.config), 0644); err != nil {
				t.Fatal(err)
			}
			_, err := (&NodeRuntime{}).OnRPC(context.Background(), proto.RPCRegisterPath, mustRaw(t, map[string]any{"path": dir, "inspect_metadata": true}))
			if err == nil || !strings.Contains(err.Error(), "config.json") || !strings.Contains(err.Error(), tc.cause) {
				t.Fatalf("recognition reason lost: %v", err)
			}
			if strings.Count(err.Error(), tc.cause) != 1 {
				t.Fatalf("repeated cause: %v", err)
			}
		})
	}
}

func TestRegisterPathPreservesAssetFailures(t *testing.T) {
	for _, tc := range []struct {
		name  string
		files map[string]string
		want  []string
	}{
		{"empty weight", map[string]string{"config.json": `{"model_type":"example"}`, "model.safetensors": ""}, []string{"model.safetensors", "file is empty"}},
		{"empty ONNX", map[string]string{"config.json": `{"model_type":"example"}`, "model.onnx": ""}, []string{"model.onnx", "file is empty"}},
		{"empty SavedModel", map[string]string{"saved_model.pb": "", "variables/variables.index": ""}, []string{"saved_model.pb", "variables/variables.index", "file is empty"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			for name, body := range tc.files {
				filename := filepath.Join(dir, name)
				if err := os.MkdirAll(filepath.Dir(filename), 0755); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filename, []byte(body), 0644); err != nil {
					t.Fatal(err)
				}
			}
			_, err := (&NodeRuntime{}).OnRPC(context.Background(), proto.RPCRegisterPath, mustRaw(t, map[string]any{"path": dir, "inspect_metadata": true}))
			if err == nil {
				t.Fatal("invalid model assets were admitted")
			}
			for _, want := range tc.want {
				if !strings.Contains(err.Error(), want) {
					t.Errorf("cause %q missing from %v", want, err)
				}
			}
		})
	}
}
