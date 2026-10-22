package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/auth"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The handler validates the request and maps the controller's verdict; the
// stop/intent protocol itself is controller.RetryCacheCopy's (its tests).
func TestCacheRetryRouteContract(t *testing.T) {
	st := modelEvictionStore(t, "cache-retry")
	model, node := modelEvictionFixture(t, st)
	storage, err := st.CreateStorage(&store.Storage{Name: "cache", Type: proto.StorageNFSCache, AllNodes: true, Enabled: true, Export: "fixture:/models", Path: "/mnt/fixture", CachePath: "/tmp/fixture-cache"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	ref := store.FileRef{ModelID: model.ID, NodeID: node.ID, StorageID: storage.ID}
	if _, err := st.PrepareModelCopy(ref, "model"); err != nil {
		t.Fatal(err)
	}
	var verdict error
	var asked []store.FileRef
	signer := modelEvictionSigner(t)
	token, err := managementSessionTestToken(t, st, signer, auth.Claims{UserID: 1, Username: "admin", Role: "admin"})
	if err != nil {
		t.Fatal(err)
	}
	prep := newTestPreparation()
	prep.retry = func(ctx context.Context, got store.FileRef) error {
		asked = append(asked, got)
		return verdict
	}
	router := Router(Deps{Store: st, AuthSigner: signer, Preparation: prep})
	path := fmt.Sprintf("/api/v1/models/%d/nodes/%s/cache/retry", model.ID, node.UUID)
	body := fmt.Sprintf(`{"storage_id":%d}`, storage.ID)
	request := func(method, path, body, auth string) *httptest.ResponseRecorder {
		req := httptest.NewRequest(method, path, bytes.NewBufferString(body))
		if auth != "" {
			req.Header.Set("Authorization", "Bearer "+auth)
		}
		out := httptest.NewRecorder()
		router.ServeHTTP(out, req)
		return out
	}
	for _, tc := range []struct {
		method, path, body, token string
		want                      int
	}{
		{"POST", path, body, "", http.StatusUnauthorized},
		{"GET", path, "", token, http.StatusMethodNotAllowed},
		{"POST", path, `{}`, token, http.StatusBadRequest},
		{"POST", path, `{`, token, http.StatusBadRequest},
		{"POST", "/api/v1/models/999999/nodes/" + node.UUID + "/cache/retry", body, token, http.StatusNotFound},
		{"POST", fmt.Sprintf("/api/v1/models/%d/nodes/missing/cache/retry", model.ID), body, token, http.StatusNotFound},
		{"POST", path, fmt.Sprintf(`{"storage_id":%d}`, storage.ID+100), token, http.StatusNotFound},
	} {
		if got := request(tc.method, tc.path, tc.body, tc.token); got.Code != tc.want {
			t.Fatalf("%s %s %s: %d %s", tc.method, tc.path, tc.body, got.Code, got.Body.String())
		}
	}
	if len(asked) != 0 {
		t.Fatal("invalid request reached the controller")
	}
	invalid := request("POST", path, `{}`, token)
	var invalidWire map[string]any
	if err := json.Unmarshal(invalid.Body.Bytes(), &invalidWire); err != nil {
		t.Fatal(err)
	}
	if invalidWire["code"] != "FIELD_INVALID" {
		t.Fatalf("wrong validation code: %s", invalid.Body.String())
	}
	for _, tc := range []struct {
		verdict error
		code    int
		wire    string
	}{
		{&planning.CacheRetryBlocked{Detail: "the previous copy has not confirmed exit"}, http.StatusConflict, ErrorModelCacheRetryBlocked},
		{store.ErrStateChanged, http.StatusConflict, ErrorModelCacheRetryBlocked},
		{store.ErrNotFound, http.StatusNotFound, ""},
		{errors.New("database is locked"), http.StatusInternalServerError, ""},
	} {
		verdict = tc.verdict
		out := request("POST", path, body, token)
		if out.Code != tc.code {
			t.Fatalf("verdict %v: %d %s", tc.verdict, out.Code, out.Body.String())
		}
		if tc.wire != "" {
			var wire map[string]any
			if err := json.Unmarshal(out.Body.Bytes(), &wire); err != nil || wire["code"] != tc.wire {
				t.Fatalf("verdict %v: wire %s", tc.verdict, out.Body.String())
			}
		}
	}
	verdict = nil
	out := request("POST", path, body, token)
	if out.Code != http.StatusAccepted || out.Body.String() != "{\"status\":\"pending\"}\n" {
		t.Fatalf("%d %s", out.Code, out.Body.String())
	}
	if asked[len(asked)-1] != ref {
		t.Fatalf("controller asked for %+v, want %+v", asked[len(asked)-1], ref)
	}
	unwired := Router(Deps{Store: st, AuthSigner: signer})
	req := httptest.NewRequest("POST", path, bytes.NewBufferString(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	unwired.ServeHTTP(rec, req)
	if rec.Code != http.StatusConflict {
		t.Fatalf("unwired retry: %d %s", rec.Code, rec.Body.String())
	}
}
