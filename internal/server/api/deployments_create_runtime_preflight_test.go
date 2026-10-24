package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

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

func TestCreateMultiNodePreflightsEveryRuntimeBeforePersistence(t *testing.T) {
	for name, test := range map[string]struct {
		image, extraArgs, code string
	}{
		"V1 on a later udocker rank":     {image: createRuntimeCompatImage, code: messages.ErrorAscendV1CompatMultiNode},
		"Engram on a later udocker rank": {image: "vllm/vllm-openai:v0.25.1", extraArgs: createRuntimeEngramArgs, code: messages.ErrorEngramSharedMemoryRuntime},
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
			requireCreateRefusalWithoutDeployment(t, h, st, string(raw), test.code)
		})
	}
}

func TestCreatePDPreflightsEveryRoleAndRankBeforePersistence(t *testing.T) {
	const multiNodeUnit = `{"mode":"vllm-nixl","roles":{"prefill":{"tp":2,"units":[{"node_uuid":"pd-a","gpus":[0],"followers":[{"node_uuid":"pd-b","gpus":[0]}]}]},"decode":{"tp":2,"units":[{"node_uuid":"pd-a","gpus":[1,2]}]}}}`
	const rolesOnDifferentRuntimes = `{"mode":"vllm-nixl","roles":{"prefill":{"tp":1,"units":[{"node_uuid":"pd-b","gpus":[0]}]},"decode":{"tp":2,"units":[{"node_uuid":"pd-a"}]}}}`
	for name, test := range map[string]struct {
		placement, image, extraArgs, code string
	}{
		"V1 on a later udocker PD rank": {
			placement: multiNodeUnit, image: createRuntimeCompatImage, code: messages.ErrorAscendV1CompatMultiNode,
		},
		"Engram on a later udocker PD role": {
			placement: rolesOnDifferentRuntimes, image: "vllm/vllm-openai:v0.25.1", extraArgs: createRuntimeEngramArgs, code: messages.ErrorEngramSharedMemoryRuntime,
		},
	} {
		t.Run(name, func(t *testing.T) {
			h, modelID, st := createRuntimePreflightFixture(t)
			var placement any
			if err := json.Unmarshal([]byte(test.placement), &placement); err != nil {
				t.Fatal(err)
			}
			body := createRuntimeBody(t, modelID, "pd-runtime", test.image, map[string]any{"pd": placement}, test.extraArgs)
			requireCreateRefusalWithoutDeployment(t, h, st, body, test.code)
		})
	}
}
