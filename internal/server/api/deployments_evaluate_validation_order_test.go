package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
)

// A draft can fail at several stages. Keep the response at the stage that
// actually answered it, including the ordered errors the form renders.
func TestEvaluateValidationStagesOverHTTP(t *testing.T) {
	r := newAutoRig(t, 1)
	for _, tc := range []struct {
		name        string
		body        string
		status      int
		fields      []string
		codes       []string
		preview     bool
		autoPlaced  bool
		healthKnown bool
		yieldKnown  bool
	}{
		{
			name:   "independent errors precede engine and stop before reporting missing model",
			body:   `{"model_id":999999,"node_uuid":"au-1","engine":"unknown","replicas":-1,"start_timeout_sec":-1,"health_check_threshold_sec":-1,"placement_strategy":"random","target_vendor":"unknown"}`,
			status: http.StatusOK, preview: true,
			fields: []string{"replicas", "start_timeout_sec", "health_check_threshold_sec", "placement_strategy", "target_vendor", "engine"},
			codes:  []string{"REPLICAS_NEGATIVE", "FIELD_INVALID", "FIELD_INVALID", "PLACEMENT_STRATEGY_INVALID", "TARGET_VENDOR_INVALID", "ENGINE_INVALID"},
		},
		{
			name:   "early field error retains preview facts and does not add missing model error",
			body:   `{"model_id":999999,"node_uuid":"au-1","engine":"vllm","replicas":-1}`,
			status: http.StatusOK, preview: true, yieldKnown: true,
			fields: []string{"replicas"}, codes: []string{"REPLICAS_NEGATIVE"},
		},
		{
			name:   "missing model belongs to its field after request validation",
			body:   `{"model_id":999999,"node_uuid":"au-1","engine":"vllm"}`,
			status: http.StatusOK, preview: true, yieldKnown: true,
			fields: []string{"model_id"}, codes: []string{"FIELD_INVALID"},
		},
		{
			name:   "known model retains health facts despite early field error",
			body:   fmt.Sprintf(`{"model_id":%d,"node_uuid":"au-1","engine":"vllm","replicas":-1}`, r.modelID),
			status: http.StatusOK, preview: true, healthKnown: true, yieldKnown: true,
			fields: []string{"replicas"}, codes: []string{"REPLICAS_NEGATIVE"},
		},
		{
			name:   "template resolution failure precedes field error response",
			body:   `{"template_id":999999,"node_uuid":"au-1","replicas":-1}`,
			status: http.StatusBadRequest,
		},
		{
			name:   "automatic placement refusal precedes all later checks",
			body:   `{"placement_mode":"auto","nnodes":2,"placement_strategy":"random","replicas":-1,"engine":"unknown","template_id":999999}`,
			status: http.StatusOK, autoPlaced: true,
			fields: []string{"placement_mode"}, codes: []string{"PLACEMENT_STRATEGY_INVALID"},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			rec := r.do(r.session, http.MethodPost, "/api/v1/evaluate", tc.body)
			if rec.Code != tc.status {
				t.Fatalf("status = %d, want %d: %s", rec.Code, tc.status, rec.Body)
			}
			var raw map[string]json.RawMessage
			if err := json.Unmarshal(rec.Body.Bytes(), &raw); err != nil {
				t.Fatal(err)
			}
			if tc.status != http.StatusOK {
				if _, ok := raw["schema_version"]; ok || raw["error"] == nil {
					t.Fatalf("hard refusal became a preview: %s", rec.Body)
				}
				return
			}
			var out dto.EvaluateResponse
			if err := json.Unmarshal(rec.Body.Bytes(), &out); err != nil {
				t.Fatal(err)
			}
			var fields, codes []string
			for _, item := range out.Errors {
				fields = append(fields, item.Field)
				codes = append(codes, item.Code)
			}
			if out.OK || out.SchemaVersion != 1 || out.AutoPlaced != tc.autoPlaced || !reflect.DeepEqual(fields, tc.fields) || !reflect.DeepEqual(codes, tc.codes) {
				t.Fatalf("ordered refusal: %s; want fields %v codes %v", rec.Body, tc.fields, tc.codes)
			}
			if tc.preview && (out.PlanScope != "per_replica" || len(out.EligibleNodes) != 1) {
				t.Fatalf("field refusal lost preview facts: %s", rec.Body)
			}
			if (out.HealthCheckable != nil) != tc.healthKnown || (out.YieldPolicyEligible != nil) != tc.yieldKnown {
				t.Fatalf("health/yield facts changed at the validation stage: %s", rec.Body)
			}
			if !tc.preview && (out.PlanScope != "" || out.HealthCheckable != nil || len(out.EligibleNodes) != 0) {
				t.Fatalf("automatic refusal ran later preview stages: %s", rec.Body)
			}
		})
	}
	if count := r.deployments(t); count != 0 {
		t.Fatalf("evaluate created %d deployments", count)
	}
}
