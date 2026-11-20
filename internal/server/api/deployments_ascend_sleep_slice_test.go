package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/accelerator"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

// A sleepable dGPU deployment on Ascend takes its default share against the
// memory limit, not the 64 GiB card: at the card-derived 0.9 an 8 GiB limit
// left the engine 0.8 GiB for what its sleep pool holds beside the KV cache,
// and it ran out during warm-up at every start. Evaluate shows the share and
// create stores it, for what the form sends: no gpu_memory_utilization unless
// the deployer typed one. Every other combination keeps the card's share.
func TestSleepableAscendSliceTakesItsDefaultShareAgainstTheLimit(t *testing.T) {
	for _, tc := range []struct {
		name    string
		backend string
		fields  string
		want    float64
	}{
		{"ascend slice that sleeps", accelerator.BackendAscendLogical, `"vram_limit_gib":8,"sleepable":true,"quick_params":{"max_model_len":512}`, 0.75},
		{"ascend slice that stays awake", accelerator.BackendAscendLogical, `"vram_limit_gib":8,"quick_params":{"max_model_len":512}`, 0.9},
		{"ascend whole card that sleeps", accelerator.BackendAscendLogical, `"sleepable":true,"quick_params":{"max_model_len":512}`, 0.9},
		{"ascend 32 GiB slice that sleeps", accelerator.BackendAscendLogical, `"vram_limit_gib":32,"sleepable":true,"quick_params":{"max_model_len":512}`, 0.9},
		{"nvidia slice that sleeps", accelerator.BackendNVIDIALogical, `"vram_limit_gib":8,"sleepable":true,"quick_params":{"max_model_len":512}`, 0.9},
		{"a stated share is kept", accelerator.BackendAscendLogical, `"vram_limit_gib":8,"sleepable":true,"quick_params":{"max_model_len":512,"gpu_memory_utilization":0.9}`, 0.9},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, modelID := partitionPoolFixture(t, map[string]string{"slice-node": tc.backend})
			body := fmt.Sprintf(`{"name":"slice-share","model_id":%d,"node_uuid":"slice-node","engine":"vllm","image":"example/vllm:test",%s}`, modelID, tc.fields)

			wire := evaluateWire(t, h, body)
			for _, field := range []string{"form_values", "effective_params"} {
				var values map[string]any
				if err := json.Unmarshal(wire[field], &values); err != nil {
					t.Fatalf("%s = %s: %v", field, wire[field], err)
				}
				if got := values["gpu_memory_utilization"]; got != tc.want {
					t.Errorf("evaluate %s gpu_memory_utilization = %v, want %v", field, got, tc.want)
				}
			}

			created := httptest.NewRecorder()
			h.createDeployment(created, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
			if created.Code != http.StatusCreated {
				t.Fatalf("create=%d: %s", created.Code, created.Body.String())
			}
			var deployment dto.Deployment
			if err := json.Unmarshal(created.Body.Bytes(), &deployment); err != nil {
				t.Fatal(err)
			}
			stored, err := h.d.Store.GetDeployment(deployment.ID)
			if err != nil {
				t.Fatal(err)
			}
			var quick map[string]any
			if err := json.Unmarshal([]byte(stored.QuickParams), &quick); err != nil {
				t.Fatalf("stored quick_params %q: %v", stored.QuickParams, err)
			}
			if got := quick["gpu_memory_utilization"]; got != tc.want {
				t.Fatalf("the created deployment stored %v, want the %v evaluate showed", got, tc.want)
			}
		})
	}
}
