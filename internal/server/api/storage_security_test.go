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
	h := &apiHandlers{d: Deps{Store: st, Server: StaticServerSettings{}, Online: func(string) bool { return true }, CallNode: func(_ context.Context, _ string, _ string, params any) (json.RawMessage, error) {
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
	router := Router(Deps{Store: st, AuthSigner: signer, Server: StaticServerSettings{}})
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
	router := Router(Deps{Store: st, AuthSigner: signer, Server: StaticServerSettings{}})
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

// A replaced denylist frees a built-in directory and denies its own entries
// on create, PATCH and probe. A stored path the new list covers does not block
// an edit that leaves it alone.
func TestStorageDeniedPathsFollowServerSettings(t *testing.T) {
	st, signer, _ := newSessionRouter(t)
	node, err := st.RegisterNode("denied-policy-node", "denied-policy-token", "node")
	if err != nil {
		t.Fatal(err)
	}
	stored, err := st.CreateStorage(&store.Storage{Name: "stored-denied", Type: proto.StorageLocal, Enabled: true, AllNodes: true, Path: "/opt/gpuworks/models"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	router := Router(Deps{Store: st, AuthSigner: signer, Server: StaticServerSettings{StorageDenied: []string{"/opt/gpuworks"}},
		Online: func(string) bool { return true },
		CallNode: func(context.Context, string, string, any) (json.RawMessage, error) {
			calls++
			return json.Marshal(proto.StorageProbeResult{OK: true})
		}})
	cookie, _ := sessionLogin(t, router, "gpuworks-test-password-2026")
	call := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	requireDenied := func(rec *httptest.ResponseRecorder, what string) {
		t.Helper()
		var body struct {
			Code string `json:"code"`
		}
		if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusBadRequest || body.Code != ErrorStoragePathInvalid {
			t.Fatalf("%s = %d %s", what, rec.Code, rec.Body.String())
		}
	}
	if rec := call(http.MethodPost, "/api/v1/storages", `{"name":"freed","type":"local","path":"/usr/local/models"}`); rec.Code != http.StatusCreated {
		t.Fatalf("built-in directory still denied after replacement = %d %s", rec.Code, rec.Body.String())
	}
	requireDenied(call(http.MethodPost, "/api/v1/storages", `{"name":"denied","type":"local","path":"/opt/gpuworks/bin"}`), "create under a configured denied path")
	endpoint := fmt.Sprintf("/api/v1/storages/%d", stored.ID)
	if rec := call(http.MethodPatch, endpoint, `{"name":"stored-renamed"}`); rec.Code != http.StatusOK {
		t.Fatalf("rename with a stored denied path = %d %s", rec.Code, rec.Body.String())
	}
	requireDenied(call(http.MethodPatch, endpoint, `{"path":"/opt/gpuworks/bin"}`), "patch into a configured denied path")
	requireDenied(call(http.MethodPost, "/api/v1/storages/probe", `{"type":"local","path":"/opt/gpuworks/bin","node_uuid":"`+node.UUID+`"}`), "probe under a configured denied path")
	if calls != 0 {
		t.Fatalf("denied probe reached the node: calls=%d", calls)
	}
	if rec := call(http.MethodPost, "/api/v1/storages/probe", `{"type":"local","path":"/usr/local/models","node_uuid":"`+node.UUID+`"}`); rec.Code != http.StatusOK || calls != 1 {
		t.Fatalf("probe of a freed directory = %d calls=%d %s", rec.Code, calls, rec.Body.String())
	}
	row, err := st.StorageByID(stored.ID)
	if err != nil || row.Name != "stored-renamed" || row.Path != "/opt/gpuworks/models" {
		t.Fatalf("stored row = %+v err=%v", row, err)
	}
}
