package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"reflect"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestDeploymentOperationsResolveCurrentModelConfig(t *testing.T) {
	st, err := store.Open(t.TempDir()+"/test.db", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("operations-node", store.HashToken("token"), "operations-node")
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := auth.NewTokenSigner("operations-secret", time.Hour)
	token, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	router := Router(Deps{Store: st, AuthSigner: signer})
	cases := []struct{ name, operations, want string }{
		{"chat", `[{"type":"openai.chat"}]`, `[{"type":"openai.chat","endpoint":"/v1/chat/completions","renderer":"chat"}]`},
		{"speech", `[{"type":"openai.audio.speech"}]`, `[{"type":"openai.audio.speech","endpoint":"/v1/audio/speech","renderer":"audio"}]`},
		// Legacy capability and deployment snapshots must not override current
		// model operations.
		{"current-over-legacy", `[{"type":"openai.rerank"}]`, `[{"type":"openai.rerank","endpoint":"/v1/rerank","renderer":"rerank"}]`},
		{"empty", `[]`, `[]`},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "test/" + tc.name, Name: tc.name, Operations: tc.operations})
			if err != nil {
				t.Fatal(err)
			}
			dep, err := st.CreateDeployment(&store.Deployment{Name: tc.name, ServedName: tc.name, ModelID: model.ID, NodeID: node.ID, State: "active", Engine: "vllm", CatalogSelectionSnapshot: `{"api":{"operations":[{"type":"openai.audio.speech"}]}}`})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.CreateInstance(&store.Instance{UUID: tc.name, DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning, DesiredStateJSON: `{"launch":{"api_contract":{"operations":[{"type":"openai.embeddings"}]}}}`}); err != nil {
				t.Fatal(err)
			}
			req := httptest.NewRequest(http.MethodGet, "/api/v1/deployments", nil)
			req.Header.Set("Authorization", "Bearer "+token)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", rec.Code, rec.Body)
			}
			var rows []struct {
				ID         uint            `json:"id"`
				Operations json.RawMessage `json:"operations"`
			}
			if err := unmarshalList(rec.Body.Bytes(), &rows); err != nil {
				t.Fatal(err)
			}
			for _, row := range rows {
				if row.ID == dep.ID {
					if string(row.Operations) != tc.want {
						t.Fatalf("operations=%s want=%s", row.Operations, tc.want)
					}
					var roundTrip []dto.DeploymentOperation
					if err := json.Unmarshal(row.Operations, &roundTrip); err != nil {
						t.Fatal(err)
					}
					got, err := json.Marshal(roundTrip)
					if err != nil || !reflect.DeepEqual(got, []byte(tc.want)) {
						t.Fatalf("round trip=%s err=%v", got, err)
					}
					return
				}
			}
			t.Fatal("deployment missing from list")
		})
	}
}
