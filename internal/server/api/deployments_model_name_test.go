package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A cleared alias keeps the model's public routing name, and the engine is
// named after the model mount rather than the public name: the string the
// gateway sends upstream for an alias-less deployment.
func TestClearedServedNamePreservesPublicNameAndNamesEngineAfterMount(t *testing.T) {
	for _, served := range []string{"", "custom-public"} {
		t.Run(served, func(t *testing.T) {
			st, err := store.Open(":memory:", bus.New())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/qwen", Revision: "main", Name: "org/qwen", WeightBytes: 1 << 30})
			if err != nil {
				t.Fatal(err)
			}
			_, err = st.RegisterNode("model-name-node", store.HashToken("token"), "model-name-node")
			if err != nil {
				t.Fatal(err)
			}
			if err := st.UpdateNodeStatus("model-name-node", proto.NodeStatus{
				Sys:  proto.SysInfo{Hostname: "model-name-node", Arch: "amd64", GPUVendor: "nvidia", Framework: "cuda", FrameworkVersion: "13.0"},
				GPUs: []proto.GPUInfo{{Index: 0, Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90", MemoryTotalBytes: 80 << 30, Healthy: true}},
			}); err != nil {
				t.Fatal(err)
			}
			body := fmt.Sprintf(`{"name":"test","model_id":%d,"node_uuid":"model-name-node","served_name":%q,"engine":"sglang","image":"lmsysorg/sglang:latest","quick_params":{"max_model_len":4096,"gpu_memory_utilization":0.92,"enable_metrics":false}}`, model.ID, served)
			handler := &apiHandlers{d: Deps{Store: st}}
			evaluation := httptest.NewRecorder()
			handler.evaluate(evaluation, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
			if evaluation.Code != http.StatusOK {
				t.Fatalf("evaluate=%d: %s", evaluation.Code, evaluation.Body.String())
			}
			var preview dto.EvaluateResponse
			if err := json.Unmarshal(evaluation.Body.Bytes(), &preview); err != nil {
				t.Fatal(err)
			}
			engineName := served
			if engineName == "" {
				engineName = preview.ModelMount
			}
			if engineName == "" {
				t.Fatalf("preview reports no model mount: %s", evaluation.Body.String())
			}
			if got := deploy.ServedModelName(proto.ResolvedLaunch{Entrypoint: preview.Entrypoint, Argv: preview.Argv}); got != engineName {
				t.Fatalf("preview engine name=%q, want %q: %s", got, engineName, evaluation.Body.String())
			}
			created := httptest.NewRecorder()
			handler.createDeployment(created, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
			if created.Code != http.StatusCreated {
				t.Fatalf("create=%d: %s", created.Code, created.Body.String())
			}
			var response dto.Deployment
			if err := json.Unmarshal(created.Body.Bytes(), &response); err != nil {
				t.Fatal(err)
			}
			public := served
			if public == "" {
				public = model.Name
			}
			if response.ServedName != public {
				t.Fatalf("public routing name=%q, want %q", response.ServedName, public)
			}
			deployment, err := st.GetDeployment(response.ID)
			if err != nil {
				t.Fatal(err)
			}
			var quick deploy.LaunchConfig
			if err := json.Unmarshal([]byte(deployment.QuickParams), &quick); err != nil {
				t.Fatal(err)
			}
			engine, _ := deploy.SelectEngine(deployment.Engine)
			launch := engine.Build(deploy.Params{ModelPath: "/model", ServedName: deployment.ServedName, Quick: quick})
			want := served
			if want == "" {
				want = "/model"
			}
			if got := deploy.ServedModelName(proto.ResolvedLaunch{Entrypoint: launch.Entrypoint, Argv: launch.Argv}); got != want {
				t.Fatalf("persisted launch engine name=%q, want %q: %v", got, want, launch.Entrypoint)
			}
		})
	}
}
