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

func TestEvaluateSleepDeclarationAgreesWithCreate(t *testing.T) {
	st, err := store.Open("file:evaluate_sleep_declaration?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "org/qwen", Revision: "main", Name: "qwen", WeightBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	h := evaluateFieldErrorFixture(t, st)
	for _, tc := range []struct {
		engine    string
		sleepable bool
		wantError bool
	}{
		{"sglang", true, true},
		{"sglang", false, false},
		{"vllm", true, false},
		{"vllm", false, false},
	} {
		t.Run(fmt.Sprintf("%s/sleepable=%t", tc.engine, tc.sleepable), func(t *testing.T) {
			body := fmt.Sprintf(`{"name":"sleep-check","model_id":%d,"node_uuid":"field-errors-node","engine":%q,"image":"example/runner:test","sleepable":%t}`, model.ID, tc.engine, tc.sleepable)
			status, raw := evaluateForFieldErrors(t, h, body)
			if status != http.StatusOK || string(raw["ok"]) != fmt.Sprint(!tc.wantError) {
				t.Fatalf("evaluate status=%d ok=%s errors=%s", status, raw["ok"], raw["errors"])
			}
			if !tc.wantError {
				return
			}
			errors := fieldErrorsOf(t, raw)
			if len(errors) != 1 || errors[0].Field != "sleepable" || errors[0].Code != "SLEEP_ENGINE_UNSUPPORTED" || errors[0].Params["engine"] != tc.engine {
				t.Fatalf("sleep error=%+v", errors)
			}
			rec := httptest.NewRecorder()
			h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
			var created struct {
				Code string `json:"code"`
			}
			if rec.Code != http.StatusBadRequest || json.Unmarshal(rec.Body.Bytes(), &created) != nil || created.Code != errors[0].Code {
				t.Fatalf("create status=%d body=%s", rec.Code, rec.Body.String())
			}
		})
	}
}
