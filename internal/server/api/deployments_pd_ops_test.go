package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/messages"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func pdOpsRequest(h http.HandlerFunc, method string, id uint, body string) *httptest.ResponseRecorder {
	req := httptest.NewRequest(method, "/", strings.NewReader(body))
	routeCtx := chi.NewRouteContext()
	routeCtx.URLParams.Add("id", fmt.Sprint(id))
	req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, routeCtx))
	rec := httptest.NewRecorder()
	h(rec, req)
	return rec
}

func createPDForOps(t *testing.T) (*apiHandlers, *store.Store, dto.Deployment) {
	t.Helper()
	h, modelID, st := pdAPIFixture(t)
	rec := httptest.NewRecorder()
	h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(pdBody(modelID, pdOneEach, ""))))
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body.String())
	}
	var out dto.Deployment
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	return h, st, out
}

func TestUpdatePDScalesRolesAndFallback(t *testing.T) {
	h, st, created := createPDForOps(t)
	rec := pdOpsRequest(h.updatePD, http.MethodPut, created.ID, `{"roles":{"prefill":{"units":[{"node_uuid":"pd-a","gpus":[0]}]}},"fallback":"fail"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("scale = %d %s", rec.Code, rec.Body.String())
	}
	var out dto.Deployment
	_ = json.Unmarshal(rec.Body.Bytes(), &out)
	if out.PD.Fallback != "fail" || out.PD.Roles[1].Role != "prefill" || out.PD.Roles[1].Desired != 1 || out.PD.Roles[0].Desired != 1 || out.PD.Roles[1].TP != 1 {
		t.Fatalf("pd = %+v", out.PD)
	}
	dep, _ := st.GetDeployment(created.ID)
	if !strings.Contains(dep.Placement, `"fallback":"fail"`) {
		t.Fatalf("stored placement = %s", dep.Placement)
	}
	// Prefill may go to zero and come back; decode may not.
	if rec := pdOpsRequest(h.updatePD, http.MethodPut, created.ID, `{"roles":{"prefill":{"units":[]}}}`); rec.Code != http.StatusOK {
		t.Fatalf("prefill to zero = %d %s", rec.Code, rec.Body.String())
	}
	if rec := pdOpsRequest(h.updatePD, http.MethodPut, created.ID, `{"roles":{"prefill":{"units":[{"node_uuid":"pd-a"}]}}}`); rec.Code != http.StatusOK {
		t.Fatalf("prefill back = %d %s", rec.Code, rec.Body.String())
	}
	for body, code := range map[string]string{
		`{"roles":{"decode":{"units":[]}}}`:                            messages.ErrorPDRoleUnitsEmpty,
		`{"roles":{"decode":{"tp":1,"units":[{"node_uuid":"pd-b"}]}}}`: messages.ErrorPDImmutable,
		`{"mode":"sglang-nixl"}`:                                       messages.ErrorPDImmutable,
		`{"kv_load_failure":"recompute"}`:                              messages.ErrorPDImmutable,
		`{"roles":{"prefill":{"units":[{"node_uuid":"nope"}]}}}`:       messages.ErrorPDNodeUnknown,
		`{"fallback":"drop"}`:                                          messages.ErrorPDFallbackInvalid,
	} {
		rec := pdOpsRequest(h.updatePD, http.MethodPut, created.ID, body)
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"`+code+`"`) {
			t.Fatalf("%s = %d %s, want %s", body, rec.Code, rec.Body.String(), code)
		}
	}
	if rec := pdOpsRequest(h.updatePD, http.MethodPut, created.ID, `{"surprise":1}`); rec.Code != http.StatusBadRequest {
		t.Fatalf("unknown field = %d", rec.Code)
	}
	if rec := pdOpsRequest(h.updatePD, http.MethodPut, 999, `{}`); rec.Code != http.StatusNotFound {
		t.Fatalf("missing deployment = %d", rec.Code)
	}
}

func mustDeployment(t *testing.T, st *store.Store, id uint) *store.Deployment {
	t.Helper()
	dep, err := st.GetDeployment(id)
	if err != nil {
		t.Fatal(err)
	}
	return dep
}
