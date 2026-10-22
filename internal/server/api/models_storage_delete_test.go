package api

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/go-chi/chi/v5"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// An instance on an nfs_cache storage reads its node's cache entry, not the
// authoritative copy: deleting the model from the storage must see that
// reader, and must not touch any file while it is refused.
func TestDeletingAModelFromACacheStorageSeesTheCacheReaders(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("gpu3", store.HashToken("t"), "gpu3")
	if err != nil {
		t.Fatal(err)
	}
	cache, err := st.CreateStorage(&store.Storage{Name: "lab-nfs", Type: proto.StorageNFSCache, Enabled: true, AllNodes: true,
		Path: "/mnt/gpuworks/lab-nfs", Export: "nfs.example.internal:/export/lab"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyStorageStatus(node.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{
		{ID: cache.ID, State: proto.StorageStateReady, Path: "/mnt/gpuworks/lab-nfs", Writable: true, CachePath: "/opt/gpuworks/cache/lab-nfs"},
	}}); err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "Qwen/Qwen2.5-0.5B-Instruct", Name: "q05"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, StorageID: cache.ID, Path: "Qwen/Qwen2.5-0.5B-Instruct", State: proto.MFPresent}); err != nil {
		t.Fatal(err)
	}
	cached, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, StorageID: cache.ID, NodeID: node.ID, Path: "/opt/gpuworks/cache/lab-nfs/q05", State: proto.MFPresent})
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&store.Deployment{Name: "q05", ServedName: "q05", ModelID: model.ID, State: store.DeployActive})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateInstances([]*store.Instance{{UUID: "q05-0", DeploymentID: dep.ID, NodeID: node.ID, State: proto.InstRunning}},
		[][]store.InstanceModelFile{{{ModelID: model.ID, ModelFileID: cached.ID, HostPath: cached.Path}}}); err != nil {
		t.Fatal(err)
	}
	var calls []string
	h := &apiHandlers{d: Deps{Store: st, Online: func(string) bool { return true },
		CallNode: func(_ context.Context, _ string, method string, _ any) (json.RawMessage, error) {
			calls = append(calls, method)
			return json.RawMessage(`{}`), nil
		}}}
	del := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodDelete, "/", nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", fmt.Sprint(model.ID))
		rctx.URLParams.Add("storage_id", fmt.Sprint(cache.ID))
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
		rec := httptest.NewRecorder()
		h.deleteModelInStorage(rec, req)
		return rec
	}
	rec := del()
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), fmt.Sprintf(`"deployment_ids":[%d]`, dep.ID)) {
		t.Fatalf("delete while a cache is read = %d %s", rec.Code, rec.Body.String())
	}
	if len(calls) != 0 {
		t.Fatalf("a refused delete called the node: %v", calls)
	}
	if row, err := st.ModelFileByID(cached.ID); err != nil || row.State != proto.MFPresent {
		t.Fatalf("cache row after a refused delete = %+v err=%v", row, err)
	}
}

// A second storage with a name already in use is a conflict the form can
// say, not an internal error; a refused storage delete leaves this control
// plane's heartbeat in place for the others to see.
func TestStorageNameConflictAndRefusedDeleteKeepTheHeartbeat(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("gpu3", store.HashToken("t"), "gpu3")
	if err != nil {
		t.Fatal(err)
	}
	nfs, err := st.CreateStorage(&store.Storage{Name: "team-nfs", Type: proto.StorageNFS, Enabled: true, AllNodes: true,
		Path: "/mnt/gpuworks/team-nfs", Export: "nfs.example.internal:/export/team"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyStorageStatus(node.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{
		{ID: nfs.ID, State: proto.StorageStateReady, Path: "/mnt/gpuworks/team-nfs", Writable: true},
	}}); err != nil {
		t.Fatal(err)
	}
	var calls []string
	h := &apiHandlers{d: Deps{Store: st, Online: func(string) bool { return true },
		CallNode: func(_ context.Context, _ string, method string, _ any) (json.RawMessage, error) {
			calls = append(calls, method)
			return json.RawMessage(`{"peers":[]}`), nil
		}}}

	if _, err := st.CreateStorage(&store.Storage{Name: "team-nfs", Type: proto.StorageLocal, Enabled: true, AllNodes: true, Path: "/data"}, nil); !errors.Is(err, store.ErrStorageNameTaken) {
		t.Fatalf("duplicate name = %v", err)
	}
	rec := httptest.NewRecorder()
	writeStoreErr(rec, fmt.Errorf("wrapped: %w", store.ErrStorageNameTaken))
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"code":"`+ErrorStorageNameTaken+`"`) {
		t.Fatalf("duplicate name response = %d %s", rec.Code, rec.Body.String())
	}

	if _, err := st.CreateDeploymentWithRequirements(&store.Deployment{Name: "d", ServedName: "d"},
		map[string][]string{"gpuworks.ai/storage": {fmt.Sprint(nfs.ID)}}); err != nil {
		t.Fatal(err)
	}
	del := func() *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodDelete, "/", nil)
		rctx := chi.NewRouteContext()
		rctx.URLParams.Add("id", fmt.Sprint(nfs.ID))
		req = req.WithContext(context.WithValue(req.Context(), chi.RouteCtxKey, rctx))
		rec := httptest.NewRecorder()
		h.deleteStorage(rec, req)
		return rec
	}
	if rec := del(); rec.Code != http.StatusConflict {
		t.Fatalf("delete a storage a deployment is limited to = %d %s", rec.Code, rec.Body.String())
	}
	if len(calls) != 0 {
		t.Fatalf("a refused delete withdrew the heartbeat: %v", calls)
	}
	if err := st.DB().Where("1 = 1").Delete(&store.DeploymentNodeRequirement{}).Error; err != nil {
		t.Fatal(err)
	}
	if rec := del(); rec.Code != http.StatusNoContent {
		t.Fatalf("delete = %d %s", rec.Code, rec.Body.String())
	}
	if len(calls) != 1 || calls[0] != proto.RPCStoragePeers {
		t.Fatalf("calls after the delete = %v, want the heartbeat withdrawn once", calls)
	}
}

// The storage form shows the default directories the server applies, and a
// trial mount of a shared storage without a path is tried where a create
// would mount it.
func TestStorageDefaultsComeFromTheServer(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	node, err := st.RegisterNode("gpu3", store.HashToken("t"), "gpu3")
	if err != nil {
		t.Fatal(err)
	}
	var probed proto.StorageProbeParams
	h := &apiHandlers{d: Deps{Store: st, Online: func(string) bool { return true },
		CallNode: func(_ context.Context, _ string, method string, params any) (json.RawMessage, error) {
			if method == proto.RPCStorageProbe {
				probed = params.(proto.StorageProbeParams)
			}
			return json.RawMessage(`{"ok":true,"readable":true,"writable":true}`), nil
		}}}
	rec := httptest.NewRecorder()
	h.getStorageSettings(rec, httptest.NewRequest(http.MethodGet, "/", nil))
	if !strings.Contains(rec.Body.String(), `"defaults":{"local_path":"/opt/gpuworks/models","mount_root":"/mnt/gpuworks","cache_root":"/opt/gpuworks/cache"}`) {
		t.Fatalf("settings = %s", rec.Body.String())
	}
	rec = httptest.NewRecorder()
	h.probeStorage(rec, httptest.NewRequest(http.MethodPost, "/", strings.NewReader(
		`{"node_uuid":"`+node.UUID+`","type":"nfs","name":"lab-nfs","export":"nfs.example.internal:/export/lab"}`)))
	if rec.Code != http.StatusOK || probed.Path != "/mnt/gpuworks/lab-nfs" {
		t.Fatalf("probe = %d %s, probed %+v", rec.Code, rec.Body.String(), probed)
	}
}
