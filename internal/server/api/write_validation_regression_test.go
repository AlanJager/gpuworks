package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"regexp"
	"strconv"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestRegisterURLModelBoundsExplicitAndDerivedNames(t *testing.T) {
	endpoint := urlModelDirectory(t)
	for _, name := range []string{strings.Repeat("A", 200000), "nul\x00name", "line\nname"} {
		t.Run(fmt.Sprint(len(name)), func(t *testing.T) {
			st := urlModelStore(t, "url_name_"+t.Name())
			h := apiHandlers{d: Deps{Store: st, Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}}}
			body, _ := json.Marshal(map[string]string{"source_url": endpoint, "name": name})
			rec := httptest.NewRecorder()
			h.registerURLModel(rec, httptest.NewRequest(http.MethodPost, "/api/v1/models/register-url", strings.NewReader(string(body))))
			if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"FIELD_INVALID"`) {
				t.Fatalf("invalid name persisted: status=%d response bytes=%d", rec.Code, rec.Body.Len())
			}
			if rows, err := st.ListModels(); err != nil || len(rows) != 0 {
				t.Fatalf("invalid registration stored %d models: %v", len(rows), err)
			}
		})
	}
	t.Run("derived", func(t *testing.T) {
		endpoint := newURLDirectoryFixture(t, "/org/"+strings.Repeat("A", 257), map[string]string{"config.json": urlModelConfig, "model.safetensors": "w"}, nil)
		st := urlModelStore(t, "url_derived_name")
		h := apiHandlers{d: Deps{Store: st, Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}}}
		body, _ := json.Marshal(map[string]string{"source_url": endpoint})
		rec := httptest.NewRecorder()
		h.registerURLModel(rec, httptest.NewRequest(http.MethodPost, "/api/v1/models/register-url", strings.NewReader(string(body))))
		if rec.Code != http.StatusBadRequest || !strings.Contains(rec.Body.String(), `"code":"FIELD_INVALID"`) {
			t.Fatalf("invalid derived name: %d %s", rec.Code, rec.Body.String())
		}
		if rows, err := st.ListModels(); err != nil || len(rows) != 0 {
			t.Fatalf("invalid derived name stored %d models: %v", len(rows), err)
		}
	})
	t.Run("Unicode boundary", func(t *testing.T) {
		st := urlModelStore(t, "url_name_boundary")
		h := apiHandlers{d: Deps{Store: st, Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}}}
		name := strings.Repeat("模", 256)
		body, _ := json.Marshal(map[string]string{"source_url": endpoint, "name": name})
		rec := httptest.NewRecorder()
		h.registerURLModel(rec, httptest.NewRequest(http.MethodPost, "/api/v1/models/register-url", strings.NewReader(string(body))))
		rows, err := st.ListModels()
		if rec.Code != http.StatusCreated || err != nil || len(rows) != 1 || rows[0].Name != name {
			t.Fatalf("valid name rejected: status=%d models=%d err=%v", rec.Code, len(rows), err)
		}
	})
}

func TestURLModelWritesRejectTrailingJSONBeforeReadingSource(t *testing.T) {
	for _, method := range []string{"preview", "register"} {
		t.Run(method, func(t *testing.T) {
			st := urlModelStore(t, "url_trailing_"+method)
			h := apiHandlers{d: Deps{Store: st}}
			// The invalid source is deliberate: malformed bodies must fail before
			// attempting a remote request, including oversized trailing values.
			body := `{"url":"http://127.0.0.1:1/model","source_url":"http://127.0.0.1:1/model"}`
			for _, trailing := range []string{` {}`, ` "` + strings.Repeat("a", 2<<20) + `"`} {
				rec := httptest.NewRecorder()
				req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(body+trailing))
				if method == "preview" {
					h.previewURLModel(rec, req)
				} else {
					h.registerURLModel(rec, req)
				}
				if (rec.Code != http.StatusBadRequest && rec.Code != http.StatusRequestEntityTooLarge) || !strings.Contains(rec.Body.String(), `"code":"FIELD_INVALID"`) {
					t.Fatalf("trailing JSON accepted: %d %s", rec.Code, rec.Body.String())
				}
			}
		})
	}
}

func TestCloneDeploymentBoundsDerivedServedName(t *testing.T) {
	for _, tc := range []struct {
		name, served, override string
		status                 int
	}{
		// The copy's served name gains "-" and four characters.
		{"boundary", strings.Repeat("a", 251), "", http.StatusCreated},
		{"overflow", strings.Repeat("a", 252), "", http.StatusBadRequest},
		{"legacy control", "legacy\x00name", "", http.StatusBadRequest},
		{"override repairs legacy", "legacy\x00name", `{"name":"repaired"}`, http.StatusCreated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st := urlModelStore(t, "clone_served_"+t.Name())
			original, err := st.CreateDeploymentWithRequirements(&store.Deployment{Name: "original", ServedName: tc.served}, nil)
			if err != nil {
				t.Fatal(err)
			}
			rctx := chi.NewRouteContext()
			rctx.URLParams.Add("id", strconv.FormatUint(uint64(original.ID), 10))
			req := httptest.NewRequest(http.MethodPost, "/", strings.NewReader(tc.override)).WithContext(context.WithValue(context.Background(), chi.RouteCtxKey, rctx))
			rec := httptest.NewRecorder()
			(&apiHandlers{d: Deps{Store: st}}).cloneDeployment(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("clone status=%d want=%d body=%s", rec.Code, tc.status, rec.Body.String())
			}
			if tc.status == http.StatusBadRequest {
				rows, err := st.ListDeployments()
				if err != nil || len(rows) != 1 || !strings.Contains(rec.Body.String(), `"field":"served_name"`) {
					t.Fatalf("invalid clone persisted: rows=%d err=%v body=%s", len(rows), err, rec.Body.String())
				}
			} else {
				var clone dto.Deployment
				if err := json.Unmarshal(rec.Body.Bytes(), &clone); err != nil {
					t.Fatal(err)
				}
				want := regexp.MustCompile("^" + regexp.QuoteMeta(tc.served) + "-[a-z0-9]{4}$")
				if tc.override != "" {
					want = regexp.MustCompile("^repaired$")
				}
				if !want.MatchString(clone.ServedName) {
					t.Fatalf("served name=%q want %s", clone.ServedName, want)
				}
			}
		})
	}
}

func TestCreateDeploymentBoundsFallbackServedNameBeforeAcquisition(t *testing.T) {
	for _, tc := range []struct {
		name, modelName, served string
		status                  int
	}{
		{"legacy overflow", strings.Repeat("a", 257), "", http.StatusBadRequest},
		{"legacy control", "legacy\x00name", "", http.StatusBadRequest},
		{"Unicode boundary", strings.Repeat("模", 256), "", http.StatusCreated},
		{"explicit override", "legacy\x00name", "valid-public-name", http.StatusCreated},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, model, node, _ := storageFallbackFixture(t)
			model.Name = tc.modelName
			if _, err := h.d.Store.UpsertModel(model); err != nil {
				t.Fatal(err)
			}
			storage, err := h.d.Store.CreateStorage(&store.Storage{Name: "download-target", Type: proto.StorageNFS,
				Enabled: true, AllNodes: true, Path: "/mnt/gpuworks/download-target", Export: "nfs.example:/download-target"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			if err := h.d.Store.ApplyStorageStatus(node.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{{
				ID: storage.ID, State: proto.StorageStateReady, Path: storage.Path, Writable: true,
			}}}); err != nil {
				t.Fatal(err)
			}
			syncs := 0
			h.d.Online = func(string) bool { return true }
			h.d.NodeSync = nodeSyncCallbacks{full: func(string) error { syncs++; return nil }}
			body, _ := json.Marshal(map[string]any{"name": "valid-deployment", "model_id": model.ID, "node_uuid": node.UUID,
				"engine": "vllm", "image": "vllm/vllm-openai:latest", "served_name": tc.served, "model_source": map[string]any{"storage_id": storage.ID}})
			rec := httptest.NewRecorder()
			h.createDeployment(rec, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(string(body))))
			if rec.Code != tc.status {
				t.Fatalf("create status=%d want=%d response bytes=%d; acquisition syncs=%d", rec.Code, tc.status, rec.Body.Len(), syncs)
			}
			if tc.status == http.StatusBadRequest {
				if !strings.Contains(rec.Body.String(), `"field":"served_name"`) {
					t.Fatalf("wrong validation error: %s", rec.Body.String())
				}
				if rows, err := h.d.Store.ListDeployments(); err != nil || len(rows) != 0 {
					t.Fatalf("invalid deployment persisted: count=%d err=%v", len(rows), err)
				}
				if _, err := h.d.Store.ModelFileAt(store.FileRef{ModelID: model.ID, StorageID: storage.ID}); !errors.Is(err, store.ErrNotFound) || syncs != 0 {
					t.Fatalf("invalid request started model acquisition: err=%v syncs=%d", err, syncs)
				}
			} else {
				var deployment dto.Deployment
				if err := json.Unmarshal(rec.Body.Bytes(), &deployment); err != nil {
					t.Fatal(err)
				}
				want := tc.served
				if want == "" {
					want = tc.modelName
				}
				if deployment.ServedName != want || syncs != 1 {
					t.Fatalf("valid deployment served_name=%q want=%q acquisition syncs=%d", deployment.ServedName, want, syncs)
				}
			}
		})
	}
}
