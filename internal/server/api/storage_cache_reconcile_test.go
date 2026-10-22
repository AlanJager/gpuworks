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

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestStorageCacheRequestAuthValidationAndDurableIntent(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	node, err := st.RegisterNode("node-cache-api", store.HashToken("t"), "cache-node")
	if err != nil {
		t.Fatal(err)
	}
	cache, err := st.CreateStorage(&store.Storage{Name: "cache-api", Type: proto.StorageNFSCache, Enabled: true, AllNodes: true, Export: "nfs:/cache-api"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	signer, member := roleTestToken(t, st, "member")
	_, admin := roleTestToken(t, st, "admin")
	router := Router(Deps{Store: st, AuthSigner: signer})
	path := fmt.Sprintf("/api/v1/storages/%d/cache/reconcile", cache.ID)
	call := func(token, method, url, body string) *httptest.ResponseRecorder {
		t.Helper()
		req := httptest.NewRequest(method, url, strings.NewReader(body))
		if token != "" {
			req.Header.Set("Authorization", "Bearer "+token)
		}
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	body := fmt.Sprintf(`{"node_uuids":[%q],"mode":"clear"}`, node.UUID)
	for _, tc := range []struct {
		token, method, url, body string
		code                     int
	}{
		{"", http.MethodPost, path, body, 401}, {member, http.MethodPost, path, body, 403},
		{admin, http.MethodGet, path, "", 405}, {admin, http.MethodPost, "/api/v1/storages/99999/cache/reconcile", body, 404},
		{admin, http.MethodPost, path, `{"node_uuids":[],"mode":"clear"}`, 400},
		{admin, http.MethodPost, path, `{"node_uuids":["missing"],"mode":"clear"}`, 400},
		{admin, http.MethodPost, path, `{"node_uuids":["node-cache-api"],"mode":"delete"}`, 400},
		{admin, http.MethodPost, path, `{"node_uuids":["node-cache-api"],"mode":"trim","oops":true}`, 400},
		{admin, http.MethodPost, path, body, 202},
	} {
		rec := call(tc.token, tc.method, tc.url, tc.body)
		if rec.Code != tc.code {
			t.Fatalf("%s %s %s: %d %s", tc.method, tc.url, tc.body, rec.Code, rec.Body.String())
		}
	}
	op, err := st.CacheOperation(cache.ID, node.ID)
	if err != nil || op.Mode != "clear" || op.State != "pending" {
		t.Fatalf("operation=%+v err=%v", op, err)
	}
}

func TestStorageCacheWireAndLegacyErrorsKeepFiles(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	defer st.Close()
	node, err := st.RegisterNode("node-cache-wire", store.HashToken("t"), "cache-node")
	if err != nil {
		t.Fatal(err)
	}
	cache, err := st.CreateStorage(&store.Storage{Name: "cache-wire", Type: proto.StorageNFSCache, Enabled: true, AllNodes: true, Export: "nfs:/cache-wire", CacheMaxBytes: 0}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateNodeStatus(node.UUID, proto.NodeStatus{PathUsage: []proto.PathUsage{{Kind: proto.PathKindStorageCache, StorageID: cache.ID, Path: cache.CachePath, TotalBytes: 1000, AvailableBytes: 800}}}); err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyStorageStatus(node.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{{ID: cache.ID, State: proto.StorageStateReady, CachePath: cache.CachePath}}}); err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Name: "model", Source: "local", SourceID: "cache-wire-model"})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{StorageID: cache.ID, ModelID: model.ID, Path: "model", State: proto.MFPresent, SizeBytes: 50}); err != nil {
		t.Fatal(err)
	}
	file, err := st.UpsertModelFile(&store.ModelFile{StorageID: cache.ID, NodeID: node.ID, ModelID: model.ID, Path: cache.CachePath + "/model", State: proto.MFPresent, SizeBytes: 50})
	if err != nil {
		t.Fatal(err)
	}
	signer, admin := roleTestToken(t, st, "admin")
	call := func(d Deps, method, url string) *httptest.ResponseRecorder {
		t.Helper()
		d.Store = st
		d.AuthSigner = signer
		router := Router(d)
		req := httptest.NewRequest(method, url, nil)
		req.Header.Set("Authorization", "Bearer "+admin)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec
	}
	url := fmt.Sprintf("/api/v1/storages/%d", cache.ID)
	rec := call(Deps{}, http.MethodGet, url+"?all_nodes=1")
	if rec.Code != 200 {
		t.Fatal(rec.Body.String())
	}
	var got dto.Storage
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if len(got.Nodes.All) != 1 || got.Nodes.All[0].Cache == nil {
		t.Fatalf("cache missing: %s", rec.Body.String())
	}
	c := got.Nodes.All[0].Cache
	if c.UsedBytes != 50 || c.ReclaimableBytes != 50 || c.ProtectedBytes != 0 || c.LimitBytes != 0 || c.State != "satisfied" {
		t.Fatalf("cache=%+v", c)
	}
	if !strings.Contains(rec.Body.String(), `"limit_bytes":0`) || !strings.Contains(rec.Body.String(), `"protected_bytes":0`) {
		t.Fatalf("zero fields omitted: %s", rec.Body.String())
	}
	// A generation has selected a ready file but not persisted its readers.
	// Legacy clear must report a retryable conflict without calling the node.
	releaseLaunch, pinned := st.TryPinNodeCaches([]uint{node.ID})
	if !pinned {
		t.Fatal("could not reserve launch")
	}
	defer releaseLaunch()
	rec = call(Deps{CallNode: func(context.Context, string, string, any) (json.RawMessage, error) {
		t.Fatal("legacy clear crossed launch reservation")
		return nil, nil
	}}, http.MethodDelete, url+"/cache")
	if rec.Code != http.StatusConflict || !strings.Contains(rec.Body.String(), `"removed":0`) || !strings.Contains(rec.Body.String(), "retry later") {
		t.Fatalf("busy clear: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := st.ModelFileByID(file.ID); err != nil {
		t.Fatal("busy clear removed index")
	}
	releaseLaunch()
	assertEvictionReserved := func() {
		t.Helper()
		if release, ok := st.TryPinNodeCaches([]uint{node.ID}); ok {
			release()
			t.Error("launch crossed in-flight legacy clear")
		}
		if _, err := st.ModelFileByID(file.ID); err != nil {
			t.Error("legacy clear removed index before acknowledgement")
		}
	}
	assertEvictionReleased := func() {
		t.Helper()
		if release, ok := st.TryPinNodeCaches([]uint{node.ID}); !ok {
			t.Fatal("legacy clear leaked eviction reservation")
		} else {
			release()
		}
	}
	for _, d := range []Deps{{}, {Online: func(string) bool { return true }, CallNode: func(context.Context, string, string, any) (json.RawMessage, error) {
		assertEvictionReserved()
		return nil, errors.New("unlink refused")
	}}} {
		rec := call(d, http.MethodDelete, url+"/cache")
		if rec.Code < 400 {
			t.Fatalf("false successful cleanup: %d %s", rec.Code, rec.Body.String())
		}
		if _, err := st.ModelFileByID(file.ID); err != nil {
			t.Fatal("failed RPC removed index")
		}
		if d.CallNode != nil {
			failed, err := st.ModelFileByID(file.ID)
			if err != nil || failed.Ready() || failed.State != proto.MFDeleting || failed.TransferID == "" {
				t.Fatalf("failed removal remained readable: %+v %v", failed, err)
			}
			if err := st.UpdateModelFileStatus(node.ID, proto.ModelFileStatus{Key: store.ModelFileKey(file.ID), State: proto.MFPresent}); err != nil {
				t.Fatal(err)
			}
			if current, err := st.ModelFileByID(file.ID); err != nil || current.Ready() {
				t.Fatalf("late status resurrected uncertain deletion: %+v %v", current, err)
			}
			if _, err := st.PrepareModelCopy(store.FileRef{StorageID: cache.ID, NodeID: node.ID, ModelID: model.ID}, file.Path); !errors.Is(err, store.ErrCacheDeletionPending) {
				t.Fatalf("uncertain deletion allowed refill: %v", err)
			}
		}
		assertEvictionReleased()
	}
	pending, err := st.ModelFileByID(file.ID)
	if err != nil {
		t.Fatal(err)
	}
	// An acknowledgement for another intent must not release this one.
	rec = call(Deps{CallNode: func(_ context.Context, _ string, _ string, params any) (json.RawMessage, error) {
		return json.Marshal(map[string]any{"removed": true, "path": params.(map[string]string)["path"], "eviction_id": "another-operation"})
	}}, http.MethodDelete, url+"/cache")
	if rec.Code != http.StatusConflict {
		t.Fatalf("wrong receipt accepted: %d %s", rec.Code, rec.Body.String())
	}
	if current, err := st.ModelFileByID(file.ID); err != nil || current.TransferID != pending.TransferID || current.State != proto.MFDeleting {
		t.Fatalf("wrong receipt changed deletion intent: %+v %v", current, err)
	}
	rec = call(Deps{CallNode: func(_ context.Context, _ string, method string, params any) (json.RawMessage, error) {
		assertEvictionReserved()
		if method != proto.RPCCacheEvict {
			t.Fatalf("unexpected RPC %s", method)
		}
		request := params.(map[string]string)
		if request["eviction_id"] != pending.TransferID {
			t.Fatalf("retry replaced deletion identity: %v", request)
		}
		return json.Marshal(map[string]any{"removed": true, "path": request["path"], "eviction_id": request["eviction_id"]})
	}}, http.MethodDelete, url+"/cache")
	if rec.Code != http.StatusOK || !strings.Contains(rec.Body.String(), `"removed":1`) || !strings.Contains(rec.Body.String(), `"freed_bytes":50`) {
		t.Fatalf("clear retry: %d %s", rec.Code, rec.Body.String())
	}
	if _, err := st.ModelFileByID(file.ID); !errors.Is(err, store.ErrNotFound) {
		t.Fatalf("clear did not remove acknowledged index: %v", err)
	}
	assertEvictionReleased()
	// A new cache path must not borrow the old directory's statfs snapshot.
	newPath := "/different/cache"
	if _, err := st.UpdateStorage(cache.ID, store.StoragePatch{CachePath: &newPath}); err != nil {
		t.Fatal(err)
	}
	rec = call(Deps{}, http.MethodGet, url+"?all_nodes=1")
	got = dto.Storage{}
	if err := json.Unmarshal(rec.Body.Bytes(), &got); err != nil {
		t.Fatal(err)
	}
	if got.Nodes.All[0].Cache.State != "pending" || got.Nodes.All[0].CacheTotalBytes != 0 {
		t.Fatalf("stale capacity=%s", rec.Body.String())
	}
}
