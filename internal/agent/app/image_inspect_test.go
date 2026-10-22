package app

import (
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"maps"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/agent/runtime"
	"gpuworks.ai/gpuworks/internal/agent/runtime/recording"
	"gpuworks.ai/gpuworks/internal/proto"
)

func TestRPCImageInspectRedactsEnv(t *testing.T) {
	for _, tc := range []struct {
		name      string
		env, want map[string]string
	}{
		{"secrets", map[string]string{"API_TOKEN": "token-canary", "PASSWORD": "password-canary", "LOG_LEVEL": "info", "CONFIG": `{"api_token":"nested-canary"}`}, map[string]string{"API_TOKEN": "<redacted>", "PASSWORD": "<redacted>", "LOG_LEVEL": "info", "CONFIG": "<redacted>"}},
		{"nil", nil, nil},
		{"empty", map[string]string{}, map[string]string{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			driver := recording.New(t.TempDir() + "/spec.json")
			original := maps.Clone(tc.env)
			driver.InjectInspection(runtime.ImageInspection{ID: "sha256:test", Entrypoint: []string{"vllm"}, Cmd: []string{"serve"}, Env: tc.env, Volumes: []string{"/model"}})
			n := NewNodeRuntime(slog.Default(), driver, t.TempDir())
			got, err := n.OnRPC(context.Background(), proto.RPCImageInspect, json.RawMessage(`{"image_ref":"repo/image:tag"}`))
			if err != nil {
				t.Fatal(err)
			}
			want := proto.ImageInspectResult{ImageRef: "repo/image:tag", ImageID: "sha256:test", Entrypoint: []string{"vllm"}, Cmd: []string{"serve"}, Env: tc.want, Volumes: []string{"/model"}}
			if !reflect.DeepEqual(got, want) {
				t.Fatalf("inspection = %+v, want %+v", got, want)
			}
			if !reflect.DeepEqual(tc.env, original) {
				t.Fatal("inspection mutated runtime environment")
			}
			if calls := driver.Calls(); !reflect.DeepEqual(calls, []string{"InspectImage(repo/image:tag)"}) {
				t.Fatalf("runtime calls = %v", calls)
			}
		})
	}
}

func TestRPCImageInspectPropagatesRuntimeError(t *testing.T) {
	driver := recording.New(t.TempDir() + "/spec.json")
	want := errors.New("image unavailable")
	driver.InjectInspectFailure(want)
	n := NewNodeRuntime(slog.Default(), driver, t.TempDir())
	got, err := n.OnRPC(context.Background(), proto.RPCImageInspect, json.RawMessage(`{"image_ref":"repo/image:tag"}`))
	if !errors.Is(err, want) || got != nil {
		t.Fatalf("inspection = %v, error = %v", got, err)
	}
}
