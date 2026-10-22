package api

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestUpdateModelInferenceConfigPersistsCanonicalJSONAndAllowsClear(t *testing.T) {
	st, err := store.Open("file:model-inference-config-api-test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "acme/model", Revision: "main", Name: "model"})
	if err != nil {
		t.Fatal(err)
	}
	signer, _ := auth.NewTokenSigner("model-inference-config-api", time.Hour)
	token, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	router := Router(Deps{Store: st, AuthSigner: signer})
	request := httptest.NewRequest(http.MethodPatch, "/api/v1/models/"+strconv.Itoa(int(model.ID))+"/inference-config", strings.NewReader(`{"operations":[{"type":" OPENAI.CHAT "}],"input_modalities":[" IMAGE ","text","image"]}`))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var response dto.Model
	if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
		t.Fatal(err)
	}
	if len(response.Operations) != 1 || response.Operations[0].Type != "openai.chat" || response.Operations[0].Endpoint != "/v1/chat/completions" {
		t.Fatalf("operations=%+v", response.Operations)
	}
	if got, want := response.InputModalities, []string{"image", "text"}; len(got) != len(want) || got[0] != want[0] || got[1] != want[1] {
		t.Fatalf("input_modalities=%v", got)
	}
	stored, _ := st.GetModel(model.ID)
	if stored.Operations != `[{"inputs":[],"request_encoding":"json","type":"openai.chat","endpoint":"/v1/chat/completions","mode":"stream"}]` {
		t.Fatalf("stored canonical operations=%s", stored.Operations)
	}
	request = httptest.NewRequest(http.MethodPatch, "/api/v1/models/"+strconv.Itoa(int(model.ID))+"/inference-config", strings.NewReader(`{"operations":[],"input_modalities":[]}`))
	request.Header.Set("Authorization", "Bearer "+token)
	request.Header.Set("Content-Type", "application/json")
	recorder = httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != http.StatusOK {
		t.Fatalf("clear status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var cleared dto.Model
	if err := json.Unmarshal(recorder.Body.Bytes(), &cleared); err != nil {
		t.Fatal(err)
	}
	if cleared.Operations == nil || len(cleared.Operations) != 0 || cleared.InputModalities == nil || len(cleared.InputModalities) != 0 {
		t.Fatalf("clear response body=%s operations=%+v modalities=%v", recorder.Body.String(), cleared.Operations, cleared.InputModalities)
	}
	stored, _ = st.GetModel(model.ID)
	if stored.Operations != "[]" || stored.InputModalities != "[]" {
		t.Fatalf("clear config operations=%s modalities=%s", stored.Operations, stored.InputModalities)
	}
}

func TestUpdateModelInferenceConfigRejectsInvalidOperationAndModality(t *testing.T) {
	st, err := store.Open("file:model-inference-config-api-invalid-test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	model, _ := st.UpsertModel(&store.Model{Source: "local", SourceID: "local/model", Revision: "local"})
	signer, _ := auth.NewTokenSigner("model-inference-config-api-invalid", time.Hour)
	token, _ := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	router := Router(Deps{Store: st, AuthSigner: signer})
	for _, body := range []string{`{"operations":[{"type":"openai.invalid"}],"input_modalities":["text"]}`, `{"operations":[{"type":"openai.chat"}],"input_modalities":["pdf"]}`} {
		request := httptest.NewRequest(http.MethodPatch, "/api/v1/models/"+strconv.Itoa(int(model.ID))+"/inference-config", strings.NewReader(body))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Code != http.StatusBadRequest || !strings.Contains(recorder.Body.String(), ErrorModelInferenceConfigInvalid) {
			t.Fatalf("body=%s status=%d", recorder.Body.String(), recorder.Code)
		}
	}
}
