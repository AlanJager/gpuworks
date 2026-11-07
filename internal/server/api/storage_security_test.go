package api

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestStorageProbeValidatesPathsBeforeNodeRPC(t *testing.T) {
	st := routerAuthStore(t)
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("storage-security", "storage-security-token", "node")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	h := &apiHandlers{d: Deps{Store: st, Server: StaticServerSettings{StorageRoots: []string{"/srv/models"}}, Online: func(string) bool { return true }, CallNode: func(_ context.Context, _ string, _ string, params any) (json.RawMessage, error) {
		p, ok := params.(proto.StorageProbeParams)
		if !ok || p.Path != "/tmp" {
			return nil, fmt.Errorf("unexpected probe parameters: %#v", params)
		}
		calls++
		return json.Marshal(proto.StorageProbeResult{OK: true})
	}}}
	for _, path := range []string{"/etc", "/etc/ssl", "/mnt/../etc", "/mnt/gpuworks/link/../etc", "/proc/1/root", "/dev", "/var/run"} {
		body, _ := json.Marshal(map[string]string{"type": "local", "path": path, "node_uuid": node.UUID})
		rec := httptest.NewRecorder()
		h.probeStorage(rec, httptest.NewRequest(http.MethodPost, "/api/v1/storages/probe", strings.NewReader(string(body))))
		if rec.Code != http.StatusBadRequest || calls != 0 {
			t.Errorf("unsafe probe %s reached node: status=%d calls=%d body=%s", path, rec.Code, calls, rec.Body.String())
		}
	}
	body, _ := json.Marshal(map[string]string{"type": "local", "path": "/tmp", "node_uuid": node.UUID})
	rec := httptest.NewRecorder()
	h.probeStorage(rec, httptest.NewRequest(http.MethodPost, "/api/v1/storages/probe", strings.NewReader(string(body))))
	var result proto.StorageProbeResult
	if err := json.Unmarshal(rec.Body.Bytes(), &result); err != nil || rec.Code != http.StatusOK || calls != 1 || !result.OK {
		t.Fatalf("custom probe: status=%d calls=%d body=%s err=%v", rec.Code, calls, rec.Body.String(), err)
	}
}

func TestStorageCacheOverridesValidatePaths(t *testing.T) {
	st, signer, _ := newSessionRouter(t)
	node, err := st.RegisterNode("root-policy-node", "root-policy-token", "node")
	if err != nil {
		t.Fatal(err)
	}
	storage, err := st.CreateStorage(&store.Storage{Name: "root-policy", Type: proto.StorageNFSCache, Enabled: true, AllNodes: true, Path: "/srv/models/share", CachePath: "/srv/models/cache", Export: "nfs:/models"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	router := Router(Deps{Store: st, AuthSigner: signer, Server: StaticServerSettings{StorageRoots: []string{"/srv/models"}}})
	cookie, _ := sessionLogin(t, router, "gpuworks-test-password-2026")
	endpoint := fmt.Sprintf("/api/v1/storages/%d/node-overrides", storage.ID)
	for _, p := range []string{"/etc", "/srv/models/../../etc"} {
		body, _ := json.Marshal(map[string]any{"node_uuids": []string{node.UUID}, "cache_path": p})
		req := httptest.NewRequest(http.MethodPatch, endpoint, strings.NewReader(string(body)))
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != 400 {
			t.Errorf("unsafe cache override %s: %d %s", p, rec.Code, rec.Body.String())
		}
	}
	rows, err := st.StorageNodes(storage.ID)
	if err != nil || len(rows) != 0 {
		t.Fatalf("invalid overrides persisted: %+v %v", rows, err)
	}
	body, _ := json.Marshal(map[string]any{"node_uuids": []string{node.UUID}, "cache_path": "/outside/node-cache"})
	req := httptest.NewRequest(http.MethodPatch, endpoint, strings.NewReader(string(body)))
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("custom cache path rejected: %d %s", rec.Code, rec.Body.String())
	}
	rows, err = st.StorageNodes(storage.ID)
	if err != nil || len(rows) != 1 || rows[0].CachePath != "/outside/node-cache" {
		t.Fatalf("custom cache override not persisted: %+v %v", rows, err)
	}
}

// storage_probe_roots no longer limits storage paths: a create whose path,
// cache directory and node targets all sit outside the configured roots is
// stored as submitted. TestStoragePatchPaths covers the same paths on PATCH.
func TestCreateStorageAcceptsPathsOutsideConfiguredRoots(t *testing.T) {
	st, signer, _ := newSessionRouter(t)
	node, err := st.RegisterNode("root-policy-node", "root-policy-token", "node")
	if err != nil {
		t.Fatal(err)
	}
	router := Router(Deps{Store: st, AuthSigner: signer, Server: StaticServerSettings{StorageRoots: []string{"/var/lib/gpuworks"}}})
	cookie, _ := sessionLogin(t, router, "gpuworks-test-password-2026")
	body := `{"name":"outside","type":"local","path":"/opt/gpuworks/models","cache_path":"/opt/gpuworks/cache",` +
		`"targets":[{"node_uuid":"` + node.UUID + `","path":"/opt/gpuworks/node","cache_path":"/opt/gpuworks/node-cache"}]}`
	req := httptest.NewRequest(http.MethodPost, "/api/v1/storages", strings.NewReader(body))
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create outside the roots = %d %s", rec.Code, rec.Body.String())
	}
	rows, err := st.ListStorages()
	if err != nil {
		t.Fatal(err)
	}
	for _, row := range rows {
		if row.Name != "outside" {
			continue
		}
		if row.Path != "/opt/gpuworks/models" || row.CachePath != "/opt/gpuworks/cache" {
			t.Fatalf("stored row = %+v", row)
		}
		targets, err := st.StorageNodes(row.ID)
		if err != nil || len(targets) != 1 || targets[0].Path != "/opt/gpuworks/node" || targets[0].CachePath != "/opt/gpuworks/node-cache" {
			t.Fatalf("stored targets = %+v err=%v", targets, err)
		}
		return
	}
	t.Fatalf("created storage missing from %+v", rows)
}
