package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"slices"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

const createRuntimeCompatImage = "quay.io/ascend/vllm-ascend:v0.30.0rc2-a3"
const createRuntimeEngramArgs = `--engram-config '{"cpu_offload":true,"dp_shared_memory":true}'`

func createRuntimePreflightFixture(t *testing.T) (*apiHandlers, uint, *store.Store) {
	t.Helper()
	h, modelID, st := pdAPIFixture(t)
	for index, uuid := range []string{"pd-a", "pd-b"} {
		status := nodeStatusTwoGPUs(uuid, index)
		status.Sys.RuntimeDriver = "docker"
		if uuid == "pd-a" {
			third := status.GPUs[1]
			third.Index = 2
			status.GPUs = append(status.GPUs, third)
		} else {
			status.Sys.RuntimeDriver = "udocker"
		}
		if err := st.UpdateNodeStatus(uuid, status); err != nil {
			t.Fatal(err)
		}
	}
	return h, modelID, st
}

func createRuntimeBody(t *testing.T, modelID uint, name, image string, placement map[string]any, extraArgs string) string {
	t.Helper()
	body := map[string]any{
		"name": name, "model_id": modelID, "engine": "vllm", "image": image,
		"quick_params": map[string]any{"max_model_len": 4096}, "placement": placement,
	}
	if extraArgs != "" {
		body["extra_args"] = extraArgs
	}
	raw, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	return string(raw)
}

func requireCreateRefusalWithoutDeployment(t *testing.T, h *apiHandlers, st *store.Store, body, code string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
	if rec.Code != http.StatusBadRequest {
		t.Fatalf("create = %d %s, want 400 %s", rec.Code, rec.Body.String(), code)
	}
	var response struct {
		Code string `json:"code"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if response.Code != code {
		t.Fatalf("create error = %s, want %s (body %s)", response.Code, code, rec.Body.String())
	}
	deployments, err := st.ListDeployments()
	if err != nil {
		t.Fatal(err)
	}
	if len(deployments) != 0 {
		t.Fatalf("refused create persisted deployments: %+v", deployments)
	}
}

func evaluateWarningCodes(t *testing.T, h *apiHandlers, body string) []string {
	t.Helper()
	rec := httptest.NewRecorder()
	h.evaluate(rec, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
	if rec.Code != http.StatusOK {
		t.Fatalf("evaluate = %d %s", rec.Code, rec.Body.String())
	}
	var response dto.EvaluateResponse
	if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Errors) != 0 {
		t.Fatalf("evaluate errors = %+v", response.Errors)
	}
	codes := make([]string, 0, len(response.Warnings))
	for _, warning := range response.Warnings {
		codes = append(codes, warning.Code)
	}
	return codes
}

func requireCreateSucceeds(t *testing.T, h *apiHandlers, st *store.Store, body string) {
	t.Helper()
	rec := httptest.NewRecorder()
	h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s, want 201", rec.Code, rec.Body.String())
	}
	deployments, err := st.ListDeployments()
	if err != nil || len(deployments) == 0 {
		t.Fatalf("created deployments = %+v, %v", deployments, err)
	}
}

// What the platform merely cannot prove is shown by evaluate and never stops
// the create: the rank on the udocker node warns, the deployment is created.
func TestCreateWithUnprovenRuntimeCombinationsWarnsAndSucceeds(t *testing.T) {
	for name, test := range map[string]struct {
		image, extraArgs, code string
	}{
		"V1 on a later udocker rank":     {image: createRuntimeCompatImage, code: messages.WarningAscendV1CompatMultiNode},
		"Engram on a later udocker rank": {image: "vllm/vllm-openai:v0.25.1", extraArgs: createRuntimeEngramArgs, code: messages.WarningEngramSharedMemoryRuntime},
	} {
		t.Run(name, func(t *testing.T) {
			h, modelID, st := createRuntimePreflightFixture(t)
			body := createRuntimeBody(t, modelID, "multi-runtime", test.image, map[string]any{"tp": 2}, test.extraArgs)
			var request map[string]any
			if err := json.Unmarshal([]byte(body), &request); err != nil {
				t.Fatal(err)
			}
			request["nnodes"] = 2
			request["node_uuid"] = "pd-a"
			request["node_uuids"] = []string{"pd-a", "pd-b"}
			raw, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			if codes := evaluateWarningCodes(t, h, string(raw)); !slices.Contains(codes, test.code) {
				t.Fatalf("evaluate warnings = %v, want %s", codes, test.code)
			}
			requireCreateSucceeds(t, h, st, string(raw))
		})
	}
}

func TestCreatePDWithUnprovenRuntimeCombinationsWarnsAndSucceeds(t *testing.T) {
	const multiNodeUnit = `{"mode":"vllm-nixl","roles":{"prefill":{"tp":2,"units":[{"node_uuid":"pd-a","gpus":[0],"followers":[{"node_uuid":"pd-b","gpus":[0]}]}]},"decode":{"tp":2,"units":[{"node_uuid":"pd-a","gpus":[1,2]}]}}}`
	const rolesOnDifferentRuntimes = `{"mode":"vllm-nixl","roles":{"prefill":{"tp":1,"units":[{"node_uuid":"pd-b","gpus":[0]}]},"decode":{"tp":2,"units":[{"node_uuid":"pd-a"}]}}}`
	for name, test := range map[string]struct {
		placement, image, extraArgs, code string
	}{
		"V1 on a later udocker PD rank": {
			placement: multiNodeUnit, image: createRuntimeCompatImage, code: messages.WarningAscendV1CompatMultiNode,
		},
		"Engram on a later udocker PD role": {
			placement: rolesOnDifferentRuntimes, image: "vllm/vllm-openai:v0.25.1", extraArgs: createRuntimeEngramArgs, code: messages.WarningEngramSharedMemoryRuntime,
		},
	} {
		t.Run(name, func(t *testing.T) {
			h, modelID, st := createRuntimePreflightFixture(t)
			var placement any
			if err := json.Unmarshal([]byte(test.placement), &placement); err != nil {
				t.Fatal(err)
			}
			body := createRuntimeBody(t, modelID, "pd-runtime", test.image, map[string]any{"pd": placement}, test.extraArgs)
			if codes := evaluateWarningCodes(t, h, body); !slices.Contains(codes, test.code) {
				t.Fatalf("evaluate warnings = %v, want %s", codes, test.code)
			}
			requireCreateSucceeds(t, h, st, body)
		})
	}
}
