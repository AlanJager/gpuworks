package api

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
)

func TestRegisterLocalModelPreservesRecognitionReasons(t *testing.T) {
	for _, tc := range []struct{ name, report, cause string }{
		{"detailed", `{"issues":[{"path":"config.json","code":"unrecognized_model","detail":"Transformers identity is absent: neither model_type nor architectures is declared"}]}`, "neither model_type nor architectures"},
		{"old empty report", `{}`, "no detailed failure reason"},
		{"missing shard", `{"entries":[{"path":".","incomplete":true,"missing":["weights-00002.safetensors"]}]}`, "weights-00002.safetensors"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, st := registerPathRig(t, "diagnostics_"+strings.ReplaceAll(tc.name, " ", "_"), func(map[string]any) string {
				return `{"path":"/models/example","size_bytes":1,"metadata":` + tc.report + `}`
			})
			rec := httptest.NewRecorder()
			h.registerLocalModel(rec, httptest.NewRequest(http.MethodPost, "/api/v1/models/local", strings.NewReader(`{"node_uuid":"node-1","path":"/models/example"}`)))
			body := rec.Body.String()
			if rec.Code != http.StatusBadRequest || !strings.Contains(body, tc.cause) {
				t.Fatalf("status=%d body=%s", rec.Code, body)
			}
			for _, guess := range []string{"upgrade", "Server version", "Agent version"} {
				if strings.Contains(body, guess) {
					t.Errorf("invented cause %q: %s", guess, body)
				}
			}
			models, err := st.ListModels()
			if err != nil || len(models) != 0 {
				t.Fatalf("rejected model persisted: %+v %v", models, err)
			}
		})
	}
}
