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
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/store"
)

const hostGiB = float64(1 << 30)

// hostBackingFixture is one node with a 64 GiB card of the given vendor and a
// model small enough to fit any share of it.
func hostBackingFixture(t *testing.T, vendor, card string) (*apiHandlers, uint) {
	t.Helper()
	st, err := store.Open("file:"+strings.NewReplacer("/", "_", " ", "_").Replace(t.Name())+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	if _, err := st.RegisterNode("host-node", store.HashToken("token"), "host-node"); err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateNodeStatus("host-node", proto.NodeStatus{
		Sys:  proto.SysInfo{Hostname: "host-node", Arch: "amd64", GPUVendor: vendor, RuntimeDriver: "docker", MemTotalKB: 62 << 20},
		GPUs: []proto.GPUInfo{{Index: 0, Name: card, Vendor: vendor, ArchID: card, MemoryTotalBytes: 64 << 30, Healthy: true}},
	}); err != nil {
		t.Fatal(err)
	}
	deploy.SetDefaultImage("vllm", "example/vllm:test")
	model, err := st.UpsertModel(&store.Model{
		Source: "huggingface", SourceID: "org/qwen", Revision: "main", Name: "org/qwen", WeightBytes: 2 << 30,
		Meta: `{"model_type":"qwen3","num_hidden_layers":1,"hidden_size":256,"num_attention_heads":8,"num_key_value_heads":8,"max_position_embeddings":2048}`,
	})
	if err != nil {
		t.Fatal(err)
	}
	return &apiHandlers{d: Deps{Store: st}}, model.ID
}

// On Hygon an engine started with sleep keeps host memory behind the GPU
// memory it takes, so a sleepable deployment whose share of the card the
// node's host memory cannot back is refused before it is created: evaluate
// answers the field error on the sleep switch and create answers 409, both
// with the stable code and the same params. The request is what the form
// sends: no gpu_memory_utilization unless the deployer typed one, so the
// refusal is taken at the default share (0.9 of 64 GiB, plus the engine's own
// 8 GiB, against 55 GiB available less 4 GiB of headroom).
func TestEvaluateAndCreateRefuseSleepTheHostCannotBackOnHygon(t *testing.T) {
	h, modelID := hostBackingFixture(t, "hygon", "K100_AI")
	h.d.MetricsLatest = metricsFor("host-node", 62*hostGiB, 7*hostGiB)
	body := fmt.Sprintf(`{"name":"hy-sleep","model_id":%d,"node_uuid":"host-node","engine":"vllm","image":"example/vllm:test","sleepable":true,"quick_params":{"max_model_len":512}}`, modelID)
	const wantParams = `{"available_gib":"55.0","need_gib":"65.6","node":"host-node","vendor":"Hygon"}`

	wire := evaluateWire(t, h, body)
	if string(wire["ok"]) != "false" {
		t.Fatalf("evaluate ok = %s, want false", wire["ok"])
	}
	var fieldErrors []struct {
		Field  string          `json:"field"`
		Code   string          `json:"code"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(wire["errors"], &fieldErrors); err != nil {
		t.Fatalf("errors = %s: %v", wire["errors"], err)
	}
	if len(fieldErrors) != 1 || fieldErrors[0].Field != "sleepable" || fieldErrors[0].Code != "SLEEP_HOST_BACKING_INSUFFICIENT" || string(fieldErrors[0].Params) != wantParams {
		t.Fatalf("evaluate errors = %s, want one SLEEP_HOST_BACKING_INSUFFICIENT on sleepable with %s", wire["errors"], wantParams)
	}
	// The deployer has to see the share the refusal is about to lower it.
	var form map[string]any
	if err := json.Unmarshal(wire["form_values"], &form); err != nil || form["gpu_memory_utilization"] != 0.9 {
		t.Fatalf("form_values = %s (%v), want gpu_memory_utilization 0.9", wire["form_values"], err)
	}

	created := httptest.NewRecorder()
	h.createDeployment(created, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
	var refusal struct {
		Code   string          `json:"code"`
		Error  string          `json:"error"`
		Params json.RawMessage `json:"params"`
	}
	if err := json.Unmarshal(created.Body.Bytes(), &refusal); err != nil {
		t.Fatalf("create body %s: %v", created.Body.String(), err)
	}
	if created.Code != http.StatusConflict || refusal.Code != "SLEEP_HOST_BACKING_INSUFFICIENT" || string(refusal.Params) != wantParams || refusal.Error == "" {
		t.Fatalf("create = %d %s, want 409 SLEEP_HOST_BACKING_INSUFFICIENT with %s", created.Code, created.Body.String(), wantParams)
	}
	if deployments, err := h.d.Store.ListDeployments(); err != nil || len(deployments) != 0 {
		t.Fatalf("a refused create left %d deployments (%v)", len(deployments), err)
	}
}

// The refusal is only for what was observed to fail: a share the host can
// back, a deployment that does not sleep, another vendor and a node whose host
// memory is not known all create as before.
func TestSleepHostBackingRefusesNothingElse(t *testing.T) {
	stale := func(string, map[string]string, int64, ...int64) ([]MetricLatest, error) { return nil, nil }
	for _, tc := range []struct {
		name, vendor, card, fields string
		metrics                    func(string, map[string]string, int64, ...int64) ([]MetricLatest, error)
	}{
		{"a share the host can back", "hygon", "K100_AI", `"sleepable":true,"quick_params":{"max_model_len":512,"gpu_memory_utilization":0.25}`, metricsFor("host-node", 62*hostGiB, 7*hostGiB)},
		{"a host with the memory", "hygon", "K100_AI", `"sleepable":true,"quick_params":{"max_model_len":512}`, metricsFor("host-node", 256*hostGiB, 7*hostGiB)},
		{"not sleepable", "hygon", "K100_AI", `"quick_params":{"max_model_len":512}`, metricsFor("host-node", 62*hostGiB, 7*hostGiB)},
		{"another vendor", "nvidia", "A100-SXM4-80GB", `"sleepable":true,"quick_params":{"max_model_len":512}`, metricsFor("host-node", 62*hostGiB, 7*hostGiB)},
		{"no metrics store", "hygon", "K100_AI", `"sleepable":true,"quick_params":{"max_model_len":512}`, nil},
		{"no recent sample", "hygon", "K100_AI", `"sleepable":true,"quick_params":{"max_model_len":512}`, stale},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, modelID := hostBackingFixture(t, tc.vendor, tc.card)
			h.d.MetricsLatest = tc.metrics
			body := fmt.Sprintf(`{"name":"hy-sleep","model_id":%d,"node_uuid":"host-node","engine":"vllm","image":"example/vllm:test",%s}`, modelID, tc.fields)
			wire := evaluateWire(t, h, body)
			if strings.Contains(string(wire["errors"]), "SLEEP_HOST_BACKING_INSUFFICIENT") {
				t.Fatalf("evaluate errors = %s", wire["errors"])
			}
			created := httptest.NewRecorder()
			h.createDeployment(created, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
			if created.Code != http.StatusCreated {
				t.Fatalf("create=%d: %s", created.Code, created.Body.String())
			}
		})
	}
}

// Previewing an edit of a deployment that already runs is not refused: its own
// engine holds the host memory the node no longer has available.
func TestSleepHostBackingDoesNotRefuseAnEditPreview(t *testing.T) {
	h, modelID := hostBackingFixture(t, "hygon", "K100_AI")
	h.d.MetricsLatest = metricsFor("host-node", 256*hostGiB, 7*hostGiB)
	body := fmt.Sprintf(`{"name":"hy-sleep","model_id":%d,"node_uuid":"host-node","engine":"vllm","image":"example/vllm:test","sleepable":true,"quick_params":{"max_model_len":512}`, modelID)
	created := httptest.NewRecorder()
	h.createDeployment(created, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body+"}")))
	var deployment struct {
		ID uint `json:"id"`
	}
	if created.Code != http.StatusCreated || json.Unmarshal(created.Body.Bytes(), &deployment) != nil || deployment.ID == 0 {
		t.Fatalf("create=%d: %s", created.Code, created.Body.String())
	}
	// The running engine now holds most of the host.
	h.d.MetricsLatest = metricsFor("host-node", 256*hostGiB, 250*hostGiB)
	if wire := evaluateWire(t, h, body+"}"); !strings.Contains(string(wire["errors"]), "SLEEP_HOST_BACKING_INSUFFICIENT") {
		t.Fatalf("a new draft on the full host: errors = %s, want the refusal", wire["errors"])
	}
	wire := evaluateWire(t, h, fmt.Sprintf(`%s,"deployment_id":%d}`, body, deployment.ID))
	if strings.Contains(string(wire["errors"]), "SLEEP_HOST_BACKING_INSUFFICIENT") {
		t.Fatalf("edit preview errors = %s", wire["errors"])
	}
}
