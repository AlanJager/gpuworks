package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A save-to preference is not a storage node requirement (ADR 0034). A new
// node outside the NFS-cache scope uses its local storage, as evaluation does.
func TestCreateFallsBackLocallyOutsidePreferredNFSCache(t *testing.T) {
	h, model, node, cache := storageFallbackFixture(t)
	body := fmt.Sprintf(`{"model_id":%d,"node_uuid":%q,"engine":"vllm","image":"vllm/vllm-openai:latest","model_source":{"storage_id":%d}}`, model.ID, node.UUID, cache.ID)
	preview := httptest.NewRecorder()
	h.evaluate(preview, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
	var evaluated dto.EvaluateResponse
	if err := json.Unmarshal(preview.Body.Bytes(), &evaluated); err != nil || preview.Code != http.StatusOK || !evaluated.OK {
		t.Fatalf("evaluate=%d %s err=%v", preview.Code, preview.Body.String(), err)
	}
	created := httptest.NewRecorder()
	h.createDeployment(created, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
	if created.Code != http.StatusCreated {
		t.Fatalf("create=%d %s; expected local fallback outside the NFS-cache scope", created.Code, created.Body.String())
	}
	var deployment dto.Deployment
	if err := json.Unmarshal(created.Body.Bytes(), &deployment); err != nil {
		t.Fatal(err)
	}
	if len(deployment.NodeRequirements["gpuworks.ai/storage"]) != 0 {
		t.Fatalf("save-to preference became a storage requirement: %s", created.Body.String())
	}
	// The same public source rule used by admission and reconciliation chooses
	// acquisition into local storage, rather than a cache fill on this node.
	source, err := h.d.Store.ModelSourceOn(model.ID, node, store.SourceRules{SaveTo: cache.ID})
	if err != nil || source.Action != store.SourceAcquire || source.Location.Storage.Type != proto.StorageLocal {
		t.Fatalf("node model source=%+v err=%v; want local acquisition", source, err)
	}
}

// With only a local target available the form hides the storage selector and
// submits a provider without storage_id. The cluster's default cache must not
// force an unconfigured node back onto NFS: the node fetches into its local
// storage when it starts, and the create itself starts no download.
func TestCreateOutsideDefaultNFSCacheFetchesIntoLocalStorageAtStart(t *testing.T) {
	h, model, node, cache := storageFallbackFixture(t)
	defaultStorage := true
	if _, err := h.d.Store.UpdateStorage(cache.ID, store.StoragePatch{IsDefault: &defaultStorage}); err != nil {
		t.Fatal(err)
	}
	body := fmt.Sprintf(`{"model_id":%d,"node_uuid":%q,"engine":"vllm","image":"vllm/vllm-openai:latest","model_source":{"provider":"huggingface"}}`, model.ID, node.UUID)
	preview := httptest.NewRecorder()
	h.evaluate(preview, httptest.NewRequest(http.MethodPost, "/api/v1/evaluate", strings.NewReader(body)))
	var evaluated dto.EvaluateResponse
	if err := json.Unmarshal(preview.Body.Bytes(), &evaluated); err != nil || preview.Code != http.StatusOK || !evaluated.OK {
		t.Fatalf("evaluate=%d %s err=%v", preview.Code, preview.Body.String(), err)
	}
	if evaluated.Acquisition == nil || len(evaluated.Acquisition.Targets) != 1 || evaluated.Acquisition.Targets[0].StorageType != proto.StorageLocal {
		t.Fatalf("expected the form's single local storage target: %s", preview.Body.String())
	}
	created := httptest.NewRecorder()
	h.createDeployment(created, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
	if created.Code != http.StatusCreated {
		t.Fatalf("create=%d %s; expected public download into local storage", created.Code, created.Body.String())
	}
	if desired, err := h.d.Store.DesiredStateForNode(node.ID); err != nil || len(desired.ModelFiles) != 0 {
		t.Fatalf("a local fetch is the controller's job at start, not the create's: tasks=%+v err=%v", desired.ModelFiles, err)
	}
	// The same rule admission and reconciliation use chooses acquisition into
	// the node's local storage, rather than a cache fill from the NFS cache.
	source, err := h.d.Store.ModelSourceOn(model.ID, node, store.SourceRules{})
	if err != nil || source.Action != store.SourceAcquire || source.Location.Storage.ID != h.d.Store.DefaultLocalStorageID() {
		t.Fatalf("node model source=%+v err=%v; want acquisition into the local storage", source, err)
	}
}

func TestStorageFallbackPreservesExplicitDestinationAndMountFailures(t *testing.T) {
	for _, tc := range []struct {
		name, source string
		configured   bool
		status       int
	}{
		{name: "configured mount failed", configured: true, status: http.StatusConflict},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, model, node, cache := storageFallbackFixture(t)
			if tc.configured {
				all := true
				if _, err := h.d.Store.UpdateStorage(cache.ID, store.StoragePatch{AllNodes: &all}); err != nil {
					t.Fatal(err)
				}
				if err := h.d.Store.ApplyStorageStatus(node.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{{
					ID: cache.ID, State: proto.StorageStateFailed, Reason: "not_mounted",
				}}}); err != nil {
					t.Fatal(err)
				}
			}
			body := fmt.Sprintf(`{"model_id":%d,"node_uuid":%q,"engine":"vllm","image":"vllm/vllm-openai:latest","model_source":{%s"storage_id":%d}}`, model.ID, node.UUID, tc.source, cache.ID)
			created := httptest.NewRecorder()
			h.createDeployment(created, httptest.NewRequest(http.MethodPost, "/api/v1/deployments", strings.NewReader(body)))
			var refusal struct {
				Code string `json:"code"`
			}
			if err := json.Unmarshal(created.Body.Bytes(), &refusal); err != nil || created.Code != tc.status || refusal.Code != ErrorStorageUnavailableOnNode {
				t.Fatalf("create=%d %s err=%v; want storage unavailable", created.Code, created.Body.String(), err)
			}
		})
	}
}

func storageFallbackFixture(t *testing.T) (*apiHandlers, *store.Model, *store.Node, *store.Storage) {
	t.Helper()
	st := newAcquisitionStore(t)
	old, err := st.RegisterNode("existing-node", store.HashToken("old-token"), "existing-node")
	if err != nil {
		t.Fatal(err)
	}
	cache, err := st.CreateStorage(&store.Storage{Name: "team-cache", Type: proto.StorageNFSCache,
		Enabled: true, Path: "/mnt/team-cache", CachePath: "/data/cache", Export: "nfs.example:/models"}, []store.StorageNode{{NodeID: old.ID}})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyStorageStatus(old.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{{
		ID: cache.ID, State: proto.StorageStateReady, Path: "/mnt/team-cache", CachePath: "/data/cache", Writable: true,
	}}}); err != nil {
		t.Fatal(err)
	}
	node, err := st.RegisterNode("new-node", store.HashToken("token"), "new-node")
	if err != nil {
		t.Fatal(err)
	}
	if err := st.UpdateNodeStatus(node.UUID, proto.NodeStatus{
		Sys:  proto.SysInfo{Hostname: node.UUID, GPUVendor: "nvidia", Framework: "cuda"},
		GPUs: []proto.GPUInfo{{Index: 0, Name: "NVIDIA H100", Vendor: "nvidia", MemoryTotalBytes: 80 << 30, Healthy: true}},
	}); err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Source: "huggingface", SourceID: "example/model", Name: "model", WeightBytes: 1 << 30})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, StorageID: cache.ID,
		Path: "example/model", State: proto.MFPresent, SizeBytes: 1 << 30}); err != nil {
		t.Fatal(err)
	}
	return &apiHandlers{d: Deps{Store: st}}, model, node, cache
}
