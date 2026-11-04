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

func TestStorageProbeRejectsSystemPathsBeforeNodeRPC(t *testing.T) {
	st := routerAuthStore(t)
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("storage-security", "storage-security-token", "node")
	if err != nil {
		t.Fatal(err)
	}
	calls := 0
	h := &apiHandlers{d: Deps{Store: st, Online: func(string) bool { return true }, CallNode: func(context.Context, string, string, any) (json.RawMessage, error) {
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
}

func TestStorageCacheOverridesRespectConfiguredRoots(t *testing.T) {
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
	for _, p := range []string{"/etc", "/srv/outside", "/srv/models-other", "/srv/models/../../etc"} {
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
	body, _ := json.Marshal(map[string]any{"node_uuids": []string{node.UUID}, "cache_path": "/srv/models/node-cache"})
	req := httptest.NewRequest(http.MethodPatch, endpoint, strings.NewReader(string(body)))
	req.AddCookie(cookie)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != 200 {
		t.Fatalf("configured cache root rejected: %d %s", rec.Code, rec.Body.String())
	}
}

// A stored path that is outside the current roots does not block a change
// that leaves the path alone. Submitting that path, or a new path outside
// the roots, is still refused and the error names the path.
func TestPatchStorageDoesNotRejudgeStoredPaths(t *testing.T) {
	st, signer, _ := newSessionRouter(t)
	node, err := st.RegisterNode("root-policy-node", "root-policy-token", "node")
	if err != nil {
		t.Fatal(err)
	}
	outside, err := st.CreateStorage(&store.Storage{
		Name: "probe-locked", Type: proto.StorageLocal, Enabled: true, AllNodes: false,
		Path: "/opt/gpuworks/models", CachePath: "/opt/gpuworks/cache",
	}, []store.StorageNode{{NodeID: node.ID, Path: "/opt/gpuworks/node", CachePath: "/opt/gpuworks/node-cache"}})
	if err != nil {
		t.Fatal(err)
	}
	router := Router(Deps{Store: st, AuthSigner: signer, Server: StaticServerSettings{StorageRoots: []string{"/var/lib/gpuworks"}}})
	cookie, _ := sessionLogin(t, router, "gpuworks-test-password-2026")
	call := func(method, path, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, path, strings.NewReader(body))
		req.AddCookie(cookie)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	endpoint := fmt.Sprintf("/api/v1/storages/%d", outside.ID)
	for _, body := range []string{
		`{"name":"probe-locked-renamed"}`,
		`{"enabled":false}`,
		`{"is_default":true}`,
	} {
		rec := call(http.MethodPatch, endpoint, body)
		if rec.Code != http.StatusOK {
			t.Fatalf("patch %s = %d %s", body, rec.Code, rec.Body.String())
		}
	}
	assertStoredPaths := func(cache string) {
		t.Helper()
		row, err := st.StorageByID(outside.ID)
		if err != nil || row.Name != "probe-locked-renamed" || row.Enabled || !row.IsDefault || row.Path != "/opt/gpuworks/models" || row.CachePath != cache {
			t.Fatalf("stored row = %+v err=%v", row, err)
		}
		targets, err := st.StorageNodes(outside.ID)
		if err != nil || len(targets) != 1 || targets[0].Path != "/opt/gpuworks/node" || targets[0].CachePath != "/opt/gpuworks/node-cache" {
			t.Fatalf("stored targets = %+v err=%v", targets, err)
		}
	}
	assertStoredPaths("/opt/gpuworks/cache")
	for _, path := range []string{"/opt/gpuworks/models", "/opt/gpuworks/cache", "/opt/gpuworks/node", "/opt/gpuworks/node-cache"} {
		var body string
		switch path {
		case "/opt/gpuworks/models":
			body = `{"path":"/opt/gpuworks/models"}`
		case "/opt/gpuworks/cache":
			body = `{"cache_path":"/opt/gpuworks/cache"}`
		case "/opt/gpuworks/node":
			body = `{"targets":[{"node_uuid":"` + node.UUID + `","path":"/opt/gpuworks/node"}]}`
		default:
			body = `{"targets":[{"node_uuid":"` + node.UUID + `","cache_path":"/opt/gpuworks/node-cache"}]}`
		}
		requireOutsideRoot(t, call(http.MethodPatch, endpoint, body), path, "/var/lib/gpuworks")
	}
	assertStoredPaths("/opt/gpuworks/cache")
	// A submitted inside cache does not pull the stored path or targets back
	// under the root check.
	rec := call(http.MethodPatch, endpoint, `{"cache_path":"/var/lib/gpuworks/cache"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("cache inside the roots = %d %s", rec.Code, rec.Body.String())
	}
	assertStoredPaths("/var/lib/gpuworks/cache")
	rec = call(http.MethodPatch, endpoint, `{"targets":[{"node_uuid":"`+node.UUID+`","path":"/var/lib/gpuworks/node","cache_path":"/var/lib/gpuworks/node-cache"}]}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("targets inside the roots = %d %s", rec.Code, rec.Body.String())
	}
	row, err := st.StorageByID(outside.ID)
	if err != nil || row.Path != "/opt/gpuworks/models" || row.CachePath != "/var/lib/gpuworks/cache" {
		t.Fatalf("path rejudged while editing targets = %+v err=%v", row, err)
	}
	rec = call(http.MethodPatch, endpoint, `{"path":"/var/lib/gpuworks/models"}`)
	if rec.Code != http.StatusOK {
		t.Fatalf("move inside the roots = %d %s", rec.Code, rec.Body.String())
	}
	requireOutsideRoot(t, call(http.MethodPost, "/api/v1/storages", `{"name":"again","type":"local","path":"/opt/gpuworks/models"}`), "/opt/gpuworks/models", "/var/lib/gpuworks")
	requireOutsideRoot(t, call(http.MethodPost, "/api/v1/storages", `{"name":"cache-out","type":"local","path":"/var/lib/gpuworks/models","cache_path":"/opt/gpuworks/cache"}`), "/opt/gpuworks/cache", "/var/lib/gpuworks")
	requireOutsideRoot(t, call(http.MethodPost, "/api/v1/storages", `{"name":"tgt-out","type":"local","path":"/var/lib/gpuworks/models","targets":[{"node_uuid":"`+node.UUID+`","path":"/opt/gpuworks/node","cache_path":"/opt/gpuworks/node-cache"}]}`), "/opt/gpuworks/node", "/var/lib/gpuworks")
}

func requireOutsideRoot(t *testing.T, rec *httptest.ResponseRecorder, path, root string) {
	t.Helper()
	var body struct {
		Code   string         `json:"code"`
		Error  string         `json:"error"`
		Params map[string]any `json:"params"`
	}
	if err := json.Unmarshal(rec.Body.Bytes(), &body); err != nil || rec.Code != http.StatusBadRequest || body.Code != ErrorStoragePathInvalid {
		t.Fatalf("outside root %s = %d %s", path, rec.Code, rec.Body.String())
	}
	if !strings.Contains(body.Error, path) || !strings.Contains(body.Error, root) || body.Params["path"] != path {
		t.Fatalf("fallback/params = %+v", body)
	}
	roots, _ := body.Params["allowed_roots"].([]any)
	for _, item := range roots {
		if item == root {
			return
		}
	}
	t.Fatalf("allowed_roots = %#v", body.Params["allowed_roots"])
}
