package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestSGLangLaunchPreparationVisibleInPreviewAndInstance(t *testing.T) {
	st, err := store.Open(":memory:", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/preview", Revision: "main", Name: "Preview", WeightBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("sglang-preview", store.HashToken("token"), "preview")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateNodeStatus(node.UUID, proto.NodeStatus{Sys: proto.SysInfo{Hostname: "preview", GPUVendor: "nvidia", Framework: "cuda"}, GPUs: []proto.GPUInfo{{Index: 0, Name: "NVIDIA H100", Vendor: "nvidia", ArchID: "sm_90", MemoryTotalBytes: 80 << 30, Healthy: true}}}); err != nil {
		t.Fatal(err)
	}
	handlers := &apiHandlers{d: Deps{Store: st}}
	for _, nodeJSON := range []string{"", `,"node_uuid":"sglang-preview"`} {
		rec := httptest.NewRecorder()
		body := fmt.Sprintf(`{"model_id":%d,"engine":"sglang","image":"lmsysorg/sglang:v0.5.14","extra_args":"--log-level error","container_overrides":{"command_mode":"engine"}%s}`, model.ID, nodeJSON)
		handlers.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
		if rec.Code != http.StatusOK {
			t.Fatalf("evaluate: %d %s", rec.Code, rec.Body.String())
		}
		var wire map[string]any
		if err := json.Unmarshal(rec.Body.Bytes(), &wire); err != nil {
			t.Fatal(err)
		}
		assertSGLangPreparationWire(t, wire)
		if nodeJSON != "" {
			ranks, ok := wire["rank_commands"].([]any)
			if !ok || len(ranks) != 1 {
				t.Fatalf("selected node must have a rank preview: %#v", wire["rank_commands"])
			}
		}
		layers := wire["env_layers"].(map[string]any)
		if layers["system"].(map[string]any)["SGLANG_LOGGING_CONFIG_PATH"] != "/tmp/gpuworks-sglang-logging.json" {
			t.Fatal("logging provenance missing")
		}
		if ranks, ok := wire["rank_commands"].([]any); ok {
			for _, rank := range ranks {
				assertSGLangPreparationWire(t, rank.(map[string]any))
			}
		}
		// Reuse the exact preview fields as an immutable desired-state snapshot;
		// instance mapping must not discard preparation or recalculate it.
		launchWire, _ := json.Marshal(wire)
		var launch proto.ResolvedLaunch
		if err := json.Unmarshal(launchWire, &launch); err != nil {
			t.Fatal(err)
		}
		snapshot, _ := json.Marshal(proto.InstanceDesiredState{ResolvedLaunch: launch})
		inst := handlers.instanceDTO(&store.Instance{NodeID: node.ID, DesiredStateJSON: string(snapshot)})
		result, _ := json.Marshal(inst)
		var instanceWire map[string]any
		_ = json.Unmarshal(result, &instanceWire)
		assertSGLangPreparationWire(t, instanceWire)
	}
}

func assertSGLangPreparationWire(t *testing.T, wire map[string]any) {
	t.Helper()
	env, ok := wire["env"].(map[string]any)
	if !ok || env["SGLANG_LOGGING_CONFIG_PATH"] != "/tmp/gpuworks-sglang-logging.json" {
		t.Fatalf("missing logging env: %#v", wire)
	}
	script, ok := wire["pre_start_script"].(string)
	if !ok || !strings.Contains(script, `"level": "ERROR"`) || !strings.Contains(script, `"$SGLANG_LOGGING_CONFIG_PATH"`) {
		t.Fatalf("missing resolved preparation: %#v", wire)
	}
	if _, exists := wire["pre_start_argv"]; exists {
		t.Fatal("script launch must not also contain preparation argv")
	}
}
