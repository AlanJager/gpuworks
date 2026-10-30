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
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestDisabledStorageWriteBoundary(t *testing.T) {
	for _, kind := range []string{proto.StorageLocal, proto.StorageNFS, proto.StorageNFSCache, proto.StorageSharedDir} {
		t.Run(kind, func(t *testing.T) {
			st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			node, err := st.RegisterNode("boundary-node", store.HashToken("token"), "node")
			if err != nil {
				t.Fatal(err)
			}
			storage, err := st.CreateStorage(&store.Storage{Name: "boundary", Type: kind, Enabled: true, AllNodes: true, Path: "/srv/boundary", Export: "nfs.example:/boundary", CachePath: "/var/cache/boundary"}, nil)
			if err != nil {
				t.Fatal(err)
			}
			// Deliberately writable beneath an administrative disable: OS permissions
			// cannot conceal a missing product-level guard.
			if err := st.ApplyStorageStatus(node.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{{ID: storage.ID, State: proto.StorageStateReady, Path: storage.Path, CachePath: storage.CachePath, Writable: true}}}); err != nil {
				t.Fatal(err)
			}
			model, err := st.UpsertModel(&store.Model{Name: "boundary-model", Source: "modelscope", SourceID: "org/boundary"})
			if err != nil {
				t.Fatal(err)
			}
			file := &store.ModelFile{StorageID: storage.ID, ModelID: model.ID, Path: "org/boundary", State: proto.MFPresent}
			if kind == proto.StorageLocal {
				file.NodeID = node.ID
			}
			if _, err := st.UpsertModelFile(file); err != nil {
				t.Fatal(err)
			}
			signer, admin := roleTestToken(t, st, "admin")
			rpcCalls := 0
			router := Router(Deps{Store: st, AuthSigner: signer, Online: func(string) bool { return true }, CallNode: func(context.Context, string, string, any) (json.RawMessage, error) {
				rpcCalls++
				return nil, fmt.Errorf("forbidden write reached RPC")
			}})
			call := func(method, path, body string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(method, path, strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer "+admin)
				rec := httptest.NewRecorder()
				router.ServeHTTP(rec, req)
				return rec
			}
			url := fmt.Sprintf("/api/v1/storages/%d", storage.ID)
			if rec := call(http.MethodPatch, url, `{"enabled":false}`); rec.Code != 200 {
				t.Fatalf("disable=%d %s", rec.Code, rec.Body)
			}
			checks := []struct{ name, method, path, body string }{
				{"model_download", http.MethodPost, fmt.Sprintf("/api/v1/models/%d/download", model.ID), fmt.Sprintf(`{"storage_id":%d,"node_uuid":%q}`, storage.ID, node.UUID)},
				{"mkdir", http.MethodPost, url + "/files/mkdir", `{"path":"files/new"}`},
				{"rename", http.MethodPost, url + "/files/rename", `{"from":"files/a","to":"files/b"}`},
				{"file_delete", http.MethodDelete, url + "/files?path=files/a", ""},
				{"upload", http.MethodPut, url + "/files/upload?path=files/a", "data"},
			}
			if kind == proto.StorageLocal {
				checks = append(checks, struct{ name, method, path, body string }{"model_delete", http.MethodDelete, fmt.Sprintf("/api/v1/models/%d/nodes/%s?storage_id=%d", model.ID, node.UUID, storage.ID), ""})
			} else {
				checks = append(checks, struct{ name, method, path, body string }{"model_delete", http.MethodDelete, fmt.Sprintf("/api/v1/models/%d/storages/%d", model.ID, storage.ID), ""})
			}
			for _, c := range checks {
				t.Run(c.name, func(t *testing.T) {
					rec := call(c.method, c.path, c.body)
					if rec.Code != 409 || !strings.Contains(rec.Body.String(), "STORAGE_DISABLED") {
						t.Errorf("want disabled refusal, got %d %s", rec.Code, rec.Body)
					}
					if rpcCalls != 0 {
						t.Errorf("forbidden RPC calls=%d", rpcCalls)
					}
				})
			}
			if kind != proto.StorageNFSCache {
				for _, c := range []struct{ method, path, body string }{{http.MethodDelete, url + "/cache", ""}, {http.MethodPost, url + "/cache/reconcile", fmt.Sprintf(`{"node_uuids":[%q],"mode":"clear"}`, node.UUID)}} {
					rec := call(c.method, c.path, c.body)
					if rec.Code != 400 || !strings.Contains(rec.Body.String(), "STORAGE_TYPE_INVALID") {
						t.Errorf("non-cache storage accepted cache operation: %d %s", rec.Code, rec.Body)
					}
				}
			}
		})
	}
}
