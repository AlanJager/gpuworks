package api

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// The read-only NFS probe is simulated; cache removal touches only the local
// copy and must preserve the source file and the shared model index.
func TestReadOnlyCacheCleanupContract(t *testing.T) {
	for _, writable := range []bool{true, false} {
		name := "read_only"
		if writable {
			name = "writable_control"
		}
		t.Run(name, func(t *testing.T) {
			st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
			if err != nil {
				t.Fatal(err)
			}
			defer st.Close()
			node, err := st.RegisterNode("readonly-cache-node", store.HashToken("t"), "cache-node")
			if err != nil {
				t.Fatal(err)
			}
			root := t.TempDir()
			source, cache := filepath.Join(root, "source"), filepath.Join(root, "cache")
			for _, p := range []string{source, cache} {
				if err := os.MkdirAll(p, 0755); err != nil {
					t.Fatal(err)
				}
			}
			cachedFile := filepath.Join(cache, "model")
			sourceFile := filepath.Join(source, "model")
			for _, p := range []string{sourceFile, cachedFile} {
				if err := os.WriteFile(p, bytes.Repeat([]byte("w"), 50), 0644); err != nil {
					t.Fatal(err)
				}
			}
			evictions := 0
			signer, admin := roleTestToken(t, st, "admin")
			router := Router(Deps{Store: st, AuthSigner: signer, Online: func(string) bool { return true }, CallNode: func(_ context.Context, uuid, method string, params any) (json.RawMessage, error) {
				if uuid != node.UUID {
					return nil, fmt.Errorf("unexpected node %s", uuid)
				}
				switch method {
				case proto.RPCStorageProbe:
					p := params.(proto.StorageProbeParams)
					if p.Type != proto.StorageNFSCache || (p.MountOptions == "ro") == writable {
						return nil, fmt.Errorf("unexpected probe: %+v", p)
					}
					return json.Marshal(proto.StorageProbeResult{OK: true, Readable: true, Writable: writable, TotalBytes: 1000, AvailableBytes: 800})
				case proto.RPCCacheEvict:
					p := params.(map[string]string)
					if p["path"] != cachedFile || p["eviction_id"] == "" {
						return nil, fmt.Errorf("unsafe deletion: %v", p)
					}
					if err := os.Remove(p["path"]); err != nil {
						return nil, err
					}
					evictions++
					return json.Marshal(map[string]any{"removed": true, "path": p["path"], "eviction_id": p["eviction_id"]})
				default:
					return nil, fmt.Errorf("unexpected RPC %s", method)
				}
			}})
			call := func(method, url, body string) *httptest.ResponseRecorder {
				req := httptest.NewRequest(method, url, strings.NewReader(body))
				req.Header.Set("Authorization", "Bearer "+admin)
				out := httptest.NewRecorder()
				router.ServeHTTP(out, req)
				return out
			}
			opts := "ro"
			if writable {
				opts = "rw"
			}
			body, _ := json.Marshal(map[string]any{"name": "readonly-cache", "type": "nfs_cache", "export": "nfs.example.internal:/models", "path": source, "cache_path": cache, "cache_max_bytes": 100, "mount_options": opts})
			res := call(http.MethodPost, "/api/v1/storages", string(body))
			var created dto.Storage
			if err := json.Unmarshal(res.Body.Bytes(), &created); err != nil || res.Code != 201 || created.Enabled != writable {
				t.Fatalf("create: %d %s %v", res.Code, res.Body, err)
			}
			t.Logf("create: HTTP %d enabled=%v", res.Code, created.Enabled)
			if err := st.ApplyStorageStatus(node.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{{ID: created.ID, State: proto.StorageStateReady, Path: source, CachePath: cache, Writable: writable}}}); err != nil {
				t.Fatal(err)
			}
			if err := st.UpdateNodeStatus(node.UUID, proto.NodeStatus{PathUsage: []proto.PathUsage{{Kind: proto.PathKindStorageCache, StorageID: created.ID, Path: cache, TotalBytes: 1000, AvailableBytes: 800}}}); err != nil {
				t.Fatal(err)
			}
			model, err := st.UpsertModel(&store.Model{Name: "model", Source: "local", SourceID: "readonly-cache-model"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := st.UpsertModelFile(&store.ModelFile{StorageID: created.ID, ModelID: model.ID, Path: "model", State: proto.MFPresent, SizeBytes: 50}); err != nil {
				t.Fatal(err)
			}
			file, err := st.UpsertModelFile(&store.ModelFile{StorageID: created.ID, NodeID: node.ID, ModelID: model.ID, Path: cachedFile, State: proto.MFPresent, SizeBytes: 50})
			if err != nil {
				t.Fatal(err)
			}
			entries, err := st.NodeCacheEntries(created.ID, node.ID)
			if err != nil || len(entries) != 1 || !entries[0].Evictable() {
				t.Fatalf("fixture is not evictable: %+v %v", entries, err)
			}
			row, err := st.StorageByID(created.ID)
			if err != nil {
				t.Fatal(err)
			}
			t.Logf("NodeFilesDeletable=%v, cache evictable=%v", store.NodeFilesDeletable(row), entries[0].Evictable())
			url := fmt.Sprintf("/api/v1/storages/%d", created.ID)
			if !writable {
				res = call(http.MethodPatch, url, `{"enabled":true}`)
				if res.Code != 409 || !strings.Contains(res.Body.String(), "STORAGE_NOT_WRITABLE") {
					t.Fatalf("enable: %d %s", res.Code, res.Body)
				}
				t.Logf("enable: HTTP %d %s", res.Code, res.Body)
			}
			t.Run("store_eviction_guard", func(t *testing.T) {
				if _, err := st.CacheEvictionFile(created.ID, node.ID, file.ID, file.Path); err != nil {
					t.Errorf("ADR0034 permits node cache eviction: %v", err)
				}
			})
			t.Run("cache_status", func(t *testing.T) {
				res := call(http.MethodGet, url+"?all_nodes=1", "")
				var got dto.Storage
				if err := json.Unmarshal(res.Body.Bytes(), &got); err != nil || len(got.Nodes.All) != 1 || got.Nodes.All[0].Cache == nil {
					t.Fatalf("status: %d %s", res.Code, res.Body)
				}
				c := got.Nodes.All[0].Cache
				t.Logf("state=%s reason=%s used=%d reclaimable=%d", c.State, c.Reason, c.UsedBytes, c.ReclaimableBytes)
				if c.State == "unavailable" {
					t.Errorf("ready local cache incorrectly unavailable: %s", c.Reason)
				}
				if c.UsedBytes != 50 || c.ReclaimableBytes != 50 {
					t.Errorf("usage changed: %+v", c)
				}
			})
			t.Run("async_clear", func(t *testing.T) {
				res := call(http.MethodPost, url+"/cache/reconcile", fmt.Sprintf(`{"node_uuids":[%q],"mode":"clear"}`, node.UUID))
				if res.Code != 202 {
					t.Errorf("want accepted: HTTP %d %s", res.Code, res.Body)
				}
			})
			t.Run("legacy_clear", func(t *testing.T) {
				res := call(http.MethodDelete, url+"/cache?node_uuid="+node.UUID, "")
				if res.Code != 200 {
					t.Errorf("want cache removed: HTTP %d %s", res.Code, res.Body)
				}
				if _, err := st.ModelFileByID(file.ID); !errors.Is(err, store.ErrNotFound) {
					t.Errorf("cache index still exists: %v", err)
				}
				if _, err := st.ModelFileAt(store.FileRef{StorageID: created.ID, ModelID: model.ID}); err != nil {
					t.Errorf("shared model index changed: %v", err)
				}
				if evictions != 1 {
					t.Errorf("eviction RPC calls=%d, want 1", evictions)
				}
				if _, err := os.Stat(cachedFile); !os.IsNotExist(err) {
					t.Errorf("local cache still exists: %v", err)
				}
				if _, err := os.Stat(sourceFile); err != nil {
					t.Errorf("authoritative file changed: %v", err)
				}
			})
		})
	}
}
