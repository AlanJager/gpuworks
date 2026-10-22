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
	cookie, _ := sessionLogin(t, router, "password")
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
