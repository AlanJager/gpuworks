package api

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/modelmeta"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestModelLogoCacheAndEndpoint(t *testing.T) {
	st, err := store.Open("file:api_model_logo_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	logoServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png; charset=binary")
		_, _ = w.Write([]byte("png-bytes"))
	}))
	defer logoServer.Close()
	logoDir := t.TempDir()
	h := &apiHandlers{d: Deps{Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}, Store: st, ModelLogoDir: logoDir}}
	name := h.cacheModelLogo(context.Background(), logoServer.URL+"/avatar.png")
	if name == "" {
		t.Fatal("cacheModelLogo returned an empty path")
	}
	if _, err := os.Stat(filepath.Join(logoDir, name)); err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "Qwen/Qwen3", Revision: "main", Name: "Qwen3", LogoPath: name})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/models/logo", nil)
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": fmt.Sprint(model.ID)}))
	rec := httptest.NewRecorder()
	h.getModelLogo(rec, req)
	if rec.Code != http.StatusOK || rec.Header().Get("Content-Type") != "image/png" || rec.Body.String() != "png-bytes" {
		t.Fatalf("status=%d content-type=%q body=%q", rec.Code, rec.Header().Get("Content-Type"), rec.Body.String())
	}
	if model.ID == 0 || strings.Contains(name, "/") {
		t.Fatalf("unexpected cached logo name=%q model=%+v", name, model)
	}
}

func TestModelLogoCacheSniffsModelScopeOctetStream(t *testing.T) {
	logoServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "application/octet-stream")
		_, _ = w.Write([]byte("\x89PNG\r\n\x1a\n\x00\x00\x00\x00"))
	}))
	defer logoServer.Close()
	logoDir := t.TempDir()
	h := &apiHandlers{d: Deps{Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}, ModelLogoDir: logoDir}}
	name := h.cacheModelLogo(context.Background(), logoServer.URL+"/avatar.png")
	if name == "" || filepath.Ext(name) != ".png" {
		t.Fatalf("cacheModelLogo = %q", name)
	}
}

func TestRefreshModelMetadataUpdatesLogoAndMetadata(t *testing.T) {
	st, err := store.Open("file:api_model_logo_refresh_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	logoServer := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		w.Header().Set("Content-Type", "image/png")
		_, _ = w.Write([]byte("fresh-logo"))
	}))
	defer logoServer.Close()
	proxy := newTunnelingProxy(t, logoServer.URL, nil)
	defer proxy.Close()
	useManagementProxy(t, st, proxy.URL)

	logoDir := t.TempDir()
	h := &apiHandlers{d: Deps{Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}, Store: st, ModelLogoDir: logoDir, FetchModelMeta: func(context.Context, string, string, string, string, []string) (*modelmeta.Meta, error) {
		return &modelmeta.Meta{README: "# refreshed", Precision: "bfloat16", LogoURL: viaProxy(logoServer.URL) + "/avatar"}, nil
	}}}
	model, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "Qwen/Qwen3", Revision: "main", Name: "Qwen3"})
	if err != nil {
		t.Fatal(err)
	}
	req := httptest.NewRequest(http.MethodPost, "/api/v1/models/1/metadata/refresh", nil)
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": fmt.Sprint(model.ID)}))
	rec := httptest.NewRecorder()
	h.refreshModelMetadata(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("status=%d body=%s", rec.Code, rec.Body.String())
	}
	updated, err := st.GetModel(model.ID)
	if err != nil || updated.README != "# refreshed" || updated.Precision != "bfloat16" || updated.LogoPath == "" {
		t.Fatalf("updated model=%+v err=%v", updated, err)
	}
	if body, err := os.ReadFile(filepath.Join(logoDir, updated.LogoPath)); err != nil || string(body) != "fresh-logo" {
		t.Fatalf("cached logo=%q err=%v", body, err)
	}
}

func TestModelLogoEndpointRejectsPathTraversal(t *testing.T) {
	st, err := store.Open("file:api_model_logo_traversal_test?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Source: "local", SourceID: "bad", Revision: "local", Name: "bad", LogoPath: "../secret"})
	if err != nil {
		t.Fatal(err)
	}
	h := &apiHandlers{d: Deps{Outside: OutsideSources{AllowPrivateHosts: []string{"127.0.0.1"}}, Store: st, ModelLogoDir: t.TempDir()}}
	req := httptest.NewRequest(http.MethodGet, "/api/v1/models/logo", nil)
	req = req.WithContext(chiRouteContext(req.Context(), map[string]string{"id": fmt.Sprint(model.ID)}))
	rec := httptest.NewRecorder()
	h.getModelLogo(rec, req)
	if rec.Code != http.StatusNotFound || model.ID == 0 {
		t.Fatalf("status=%d", rec.Code)
	}
}
