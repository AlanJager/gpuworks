package api

import (
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func requireNoRowsWritten(t *testing.T, st *store.Store) {
	t.Helper()
	models, err := st.ListModels()
	if err != nil || len(models) != 0 {
		t.Fatalf("refused create persisted models: %+v, %v", models, err)
	}
}

// The refusals that no choice of the user can avoid still stop the create,
// even though the request names no image: the template supplies the
// compatibility image and the pinned node supplies the runtime.
func TestCreateRefusesUnrunnableCompatLaunchesFromTheTemplateImage(t *testing.T) {
	v2 := `{"VLLM_USE_V2_MODEL_RUNNER":"1"}`
	for name, test := range map[string]struct {
		mutate func(*store.InferenceSpecRow)
		code   string
	}{
		"V1 with argv preparation": {
			mutate: func(spec *store.InferenceSpecRow) { spec.PreStartArgv = `["prepare-tool"]` },
			code:   messages.ErrorAscendV1CompatPreStart,
		},
		"V2 with argv preparation": {
			mutate: func(spec *store.InferenceSpecRow) { spec.PreStartArgv, spec.Env = `["prepare-tool"]`, v2 },
			code:   messages.ErrorAscendCompatPreStartArgv,
		},
		"V1 with script preparation": {
			mutate: func(spec *store.InferenceSpecRow) { spec.PreStartScript = "echo prepare" },
			code:   messages.ErrorAscendV1CompatPreStart,
		},
		"merged script above the agent limit": {
			mutate: func(spec *store.InferenceSpecRow) {
				spec.Env, spec.PreStartScript = v2, "# "+strings.Repeat("x", 64<<10)+"\n"
			},
			code: messages.ErrorPreStartScriptTooLarge,
		},
	} {
		t.Run(name, func(t *testing.T) {
			rig := newCompatTemplateRig(t, "udocker", test.mutate)
			body := rig.body(map[string]any{"node_uuid": rig.nodeID})
			requireCreateRefusalWithoutDeployment(t, rig.h, rig.st, body, test.code)
			requireNoRowsWritten(t, rig.st)
		})
	}
	t.Run("the same template on Docker is created", func(t *testing.T) {
		rig := newCompatTemplateRig(t, "docker", func(spec *store.InferenceSpecRow) { spec.PreStartArgv = `["prepare-tool"]` })
		requireCreateSucceeds(t, rig.h, rig.st, rig.body(map[string]any{"node_uuid": rig.nodeID}))
	})
}

// An unpinned create has no runtime to judge: the runtime-dependent rules wait
// for the controller, which checks the node it picks.
func TestCreatePreflightWithoutANodeSkipsRuntimeRules(t *testing.T) {
	engine, err := deploy.SelectEngine("vllm")
	if err != nil {
		t.Fatal(err)
	}
	base := deploy.ResolveLaunchInput{
		Engine: engine, Image: "quay.io/ascend/vllm-ascend:v0.30.0rc2-a3", PreStartArgv: []string{"prepare-tool"},
		Params:          deploy.Params{ModelPath: "/model", APIPort: 30000, AssignedGPUs: 1, TensorParallel: 1},
		AllowUnresolved: true,
	}
	imageFor := func(*store.Node) string { return base.Image }
	if err := preflightCreateRuntimes(base, nil, nil, "/model", nil, nil, nil, imageFor); err != nil {
		t.Fatalf("unpinned create was refused: %v", err)
	}
	pinned := &store.Node{UUID: "tpl-node", RuntimeDriver: "udocker"}
	err = preflightCreateRuntimes(base, nil, nil, "/model", pinned, nil, nil, imageFor)
	var product *messages.ProductError
	if !errors.As(err, &product) || product.Code() != messages.ErrorAscendV1CompatPreStart {
		t.Fatalf("pinned udocker create = %v, want %s", err, messages.ErrorAscendV1CompatPreStart)
	}
	// The request's own image field is empty: the node's resolved image is what
	// decides.
	base.Image = ""
	err = preflightCreateRuntimes(base, nil, nil, "/model", pinned, nil, nil, func(*store.Node) string {
		return "quay.io/ascend/vllm-ascend:v0.30.0rc2-a3"
	})
	if !errors.As(err, &product) || product.Code() != messages.ErrorAscendV1CompatPreStart {
		t.Fatalf("create with a resolved compat image = %v, want %s", err, messages.ErrorAscendV1CompatPreStart)
	}
}

func TestCreateWireStillReportsTheKeptRefusalAsProductError(t *testing.T) {
	rig := newCompatTemplateRig(t, "udocker", func(spec *store.InferenceSpecRow) { spec.PreStartArgv = `["prepare-tool"]` })
	rec := httptest.NewRecorder()
	rig.h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(rig.body(map[string]any{"node_uuid": rig.nodeID}))))
	var response struct {
		Code   string         `json:"code"`
		Error  string         `json:"error"`
		Params map[string]any `json:"params"`
	}
	if rec.Code != http.StatusBadRequest || json.Unmarshal(rec.Body.Bytes(), &response) != nil {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	if response.Code != messages.ErrorAscendV1CompatPreStart || response.Params["field"] != "pre_start_argv" ||
		!strings.Contains(response.Error, "pre_start_argv") {
		t.Fatalf("wire error = %+v", response)
	}
}
