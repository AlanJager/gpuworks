package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestStoragePatchPaths(t *testing.T) {
	for _, tc := range []struct {
		name        string
		storageType string
		legacy      bool
		body        string
		status      int
		update      func(*store.Storage)
		targetPath  string
		targetCache string
	}{
		{name: "rename", storageType: proto.StorageLocal, legacy: true, body: `{"name":"renamed"}`, status: 200,
			update: func(s *store.Storage) { s.Name = "renamed" }},
		{name: "disable", storageType: proto.StorageLocal, legacy: true, body: `{"enabled":false}`, status: 200,
			update: func(s *store.Storage) { s.Enabled = false }},
		{name: "set_default", storageType: proto.StorageLocal, legacy: true, body: `{"is_default":true}`, status: 200,
			update: func(s *store.Storage) { s.IsDefault = true }},
		{name: "cache_limit", storageType: proto.StorageNFSCache, legacy: true, body: `{"cache_max_bytes":1024}`, status: 200,
			update: func(s *store.Storage) { s.CacheMaxBytes = 1024 }},
		{name: "path_with_unchanged_legacy_cache", storageType: proto.StorageNFSCache, legacy: true, body: `{"path":"/srv/models/share"}`, status: 200,
			update: func(s *store.Storage) { s.Path = "/srv/models/share" }},
		{name: "cache_with_unchanged_legacy_path", storageType: proto.StorageNFSCache, legacy: true, body: `{"cache_path":"/srv/models/cache"}`, status: 200,
			update: func(s *store.Storage) { s.CachePath = "/srv/models/cache" }},
		{name: "node_path_with_unchanged_legacy_paths", storageType: proto.StorageNFSCache, legacy: true,
			body: `{"targets":[{"node_uuid":"patch-path-node","path":"/srv/models/node"}]}`, status: 200, targetPath: "/srv/models/node"},
		{name: "outside_path", storageType: proto.StorageLocal, body: `{"name":"renamed","path":"/outside/models"}`, status: 200,
			update: func(s *store.Storage) { s.Name, s.Path = "renamed", "/outside/models" }},
		{name: "outside_cache", storageType: proto.StorageNFSCache, body: `{"name":"renamed","cache_path":"/outside/cache"}`, status: 200,
			update: func(s *store.Storage) { s.Name, s.CachePath = "renamed", "/outside/cache" }},
		{name: "resubmitted_legacy_path", storageType: proto.StorageLocal, legacy: true,
			body: `{"name":"renamed","path":"/opt/gpuworks/models"}`, status: 200,
			update: func(s *store.Storage) { s.Name = "renamed" }},
		{name: "resubmitted_legacy_cache", storageType: proto.StorageNFSCache, legacy: true,
			body: `{"name":"renamed","cache_path":"/opt/gpuworks/cache/legacy"}`, status: 200,
			update: func(s *store.Storage) { s.Name = "renamed" }},
		{name: "similar_prefix", storageType: proto.StorageLocal, body: `{"name":"renamed","path":"/srv/models-other"}`, status: 200,
			update: func(s *store.Storage) { s.Name, s.Path = "renamed", "/srv/models-other" }},
		{name: "system_path", storageType: proto.StorageLocal, body: `{"name":"renamed","path":"/etc"}`, status: 400},
		{name: "outside_node_path", storageType: proto.StorageNFSCache,
			body: `{"name":"renamed","targets":[{"node_uuid":"patch-path-node","path":"/outside/node"}]}`, status: 200, targetPath: "/outside/node",
			update: func(s *store.Storage) { s.Name = "renamed" }},
		{name: "outside_node_cache", storageType: proto.StorageNFSCache,
			body: `{"name":"renamed","targets":[{"node_uuid":"patch-path-node","cache_path":"/outside/cache"}]}`, status: 200, targetCache: "/outside/cache",
			update: func(s *store.Storage) { s.Name = "renamed" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			st, signer, _ := newSessionRouter(t)
			node, err := st.RegisterNode("patch-path-node", "patch-path-token", "node")
			if err != nil {
				t.Fatal(err)
			}
			row := &store.Storage{Name: "legacy", Type: tc.storageType, Enabled: true, AllNodes: true, Path: "/srv/models/original"}
			if tc.storageType == proto.StorageNFSCache {
				row.Export, row.CachePath = "nfs:/models", "/srv/models/original-cache"
			}
			if tc.legacy {
				row.Path = "/opt/gpuworks/models"
				if tc.storageType == proto.StorageNFSCache {
					row.CachePath = "/opt/gpuworks/cache/legacy"
				}
			}
			row, err = st.CreateStorage(row, nil)
			if err != nil {
				t.Fatal(err)
			}
			previousDefault, err := st.DefaultStorage()
			if err != nil {
				t.Fatal(err)
			}
			// The stored definition predates the server's current root policy.
			router := Router(Deps{Store: st, AuthSigner: signer, Server: StaticServerSettings{}})
			cookie, _ := sessionLogin(t, router, "gpuworks-test-password-2026")
			req := httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/v1/storages/%d", row.ID), strings.NewReader(tc.body))
			req.AddCookie(cookie)
			rec := httptest.NewRecorder()
			router.ServeHTTP(rec, req)
			if rec.Code != tc.status {
				t.Fatalf("PATCH %s = %d %s, want %d", tc.body, rec.Code, rec.Body.String(), tc.status)
			}
			if tc.status == http.StatusBadRequest {
				var response struct {
					Code string `json:"code"`
				}
				if err := json.Unmarshal(rec.Body.Bytes(), &response); err != nil || response.Code != ErrorStoragePathInvalid {
					t.Fatalf("unexpected path error: %s, err=%v", rec.Body.String(), err)
				}
			}
			want := *row
			if tc.update != nil {
				tc.update(&want)
			}
			got, err := st.StorageByID(row.ID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.status == http.StatusOK {
				want.UpdatedAt = got.UpdatedAt
			}
			if !reflect.DeepEqual(*got, want) {
				t.Fatalf("persisted storage = %+v, want %+v", *got, want)
			}
			targets, err := st.StorageNodes(row.ID)
			if err != nil {
				t.Fatal(err)
			}
			if tc.targetPath != "" || tc.targetCache != "" {
				if len(targets) != 1 || targets[0].NodeID != node.ID || targets[0].Path != tc.targetPath || targets[0].CachePath != tc.targetCache {
					t.Fatalf("persisted targets = %+v", targets)
				}
			} else if len(targets) != 0 {
				t.Fatalf("unexpected persisted targets: %+v", targets)
			}
			currentDefault, err := st.DefaultStorage()
			if err != nil {
				t.Fatal(err)
			}
			wantDefaultID := previousDefault.ID
			if want.IsDefault {
				wantDefaultID = row.ID
			}
			if currentDefault.ID != wantDefaultID {
				t.Fatalf("default storage = %d, want %d", currentDefault.ID, wantDefaultID)
			}
		})
	}
}
