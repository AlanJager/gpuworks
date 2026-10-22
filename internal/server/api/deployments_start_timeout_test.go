package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A deployment's own instance health budget travels as start_timeout_sec:
// evaluation reports an out-of-range value beside the form control, create
// refuses it with the same code, and an accepted value is stored on the
// deployment and read back under the same key. No value means the cluster
// default applies, and the deployment carries no start_timeout_sec at all.
func TestDeploymentStartTimeoutRequestContract(t *testing.T) {
	t.Run("evaluate", func(t *testing.T) {
		st, err := store.Open("file:evaluate_start_timeout?mode=memory&cache=shared", bus.New())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = st.Close() })
		model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/start-timeout", Revision: "main", Name: "start-timeout", WeightBytes: 1 << 30})
		if err != nil {
			t.Fatal(err)
		}
		h := evaluateFieldErrorFixture(t, st)
		for _, tc := range []struct {
			value     int
			wantError bool
		}{{0, false}, {store.DeploymentStartTimeoutSecLo, false}, {3600, false}, {store.DeploymentStartTimeoutSecHi, false},
			{store.DeploymentStartTimeoutSecLo - 1, true}, {store.DeploymentStartTimeoutSecHi + 1, true}, {-1, true}} {
			status, raw := evaluateForFieldErrors(t, h, fmt.Sprintf(`{"model_id":%d,"node_uuid":"field-errors-node","engine":"vllm","start_timeout_sec":%d}`, model.ID, tc.value))
			if status != http.StatusOK {
				t.Fatalf("start_timeout_sec %d: status = %d", tc.value, status)
			}
			var found bool
			for _, fieldErr := range fieldErrorsOf(t, raw) {
				if fieldErr.Field == "start_timeout_sec" {
					found = true
					if fieldErr.Code != ErrorFieldInvalid || !strings.Contains(fmt.Sprint(fieldErr.Params["detail"]), "between 60 and 86400") {
						t.Fatalf("start_timeout_sec %d: error = %+v", tc.value, fieldErr)
					}
				}
			}
			if found != tc.wantError {
				t.Fatalf("start_timeout_sec %d: field error present = %v, want %v (%s)", tc.value, found, tc.wantError, raw["errors"])
			}
		}
	})

	t.Run("create", func(t *testing.T) {
		h, st := automaticPlacementFixture(t)
		modelID := automaticPlacementModel(t, st)
		creates := 0
		create := func(extra string) (int, map[string]any) {
			rec := httptest.NewRecorder()
			// Each create states its own served name: a second deployment may
			// not answer under the name the first holds.
			creates++
			body := fmt.Sprintf(`{"name":"timeout","served_name":"timeout-%d","model_id":%d,"engine":"vllm","image":"example/vllm:test","node_uuid":"auto-big","quick_params":{}%s}`, creates, modelID, extra)
			h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
			var out map[string]any
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatalf("body is not JSON: %s", rec.Body)
			}
			return rec.Code, out
		}

		status, out := create(`,"start_timeout_sec":30`)
		if status != http.StatusBadRequest || out["code"] != ErrorFieldInvalid {
			t.Fatalf("out-of-range create = %d %v, want 400 %s", status, out, ErrorFieldInvalid)
		}
		var count int64
		if err := st.DB().Model(&store.Deployment{}).Count(&count).Error; err != nil || count != 0 {
			t.Fatalf("a refused create stored %d deployments (err=%v)", count, err)
		}

		status, out = create(`,"start_timeout_sec":3600`)
		if status != http.StatusCreated || out["start_timeout_sec"] != float64(3600) {
			t.Fatalf("create = %d start_timeout_sec=%v", status, out["start_timeout_sec"])
		}
		row, err := st.GetDeployment(uint(out["id"].(float64)))
		if err != nil || row.StartTimeoutSec != 3600 {
			t.Fatalf("stored start_timeout_sec = %+v err=%v", row, err)
		}

		status, out = create("")
		if status != http.StatusCreated {
			t.Fatalf("create without a budget = %d %v", status, out)
		}
		if _, present := out["start_timeout_sec"]; present {
			t.Fatalf("a deployment that states no budget carries start_timeout_sec=%v", out["start_timeout_sec"])
		}
	})
}
