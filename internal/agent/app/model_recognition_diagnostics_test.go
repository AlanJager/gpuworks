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
