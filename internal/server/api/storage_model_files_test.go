package api

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// A storage's detail page lists what it holds: the shared copy first, then
// each node's cache with when it was last used, and the deployments bound to
// the storage that use the model. Its node list carries each node's capacity.
func TestStorageDetailListsModelFilesAndNodeCapacity(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	signer, member := roleTestToken(t, st, "member")
	router := Router(Deps{Store: st, AuthSigner: signer})
	get := func(path string) (int, string) {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+member)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		return rec.Code, rec.Body.String()
	}
	gpu3, err := st.RegisterNode("gpu3", store.HashToken("a"), "gpu3")
	if err != nil {
		t.Fatal(err)
	}
	gpu2, err := st.RegisterNode("gpu2", store.HashToken("b"), "gpu2")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []*store.Node{gpu3, gpu2} {
		if err := st.SetNodeState(n.UUID, store.NodeStateOnline); err != nil {
			t.Fatal(err)
		}
	}
	cache, err := st.CreateStorage(&store.Storage{Name: "team-cache", Type: proto.StorageNFSCache, Enabled: true, AllNodes: true,
		Path: "/mnt/gpuworks/team-cache", Export: "nfs.example.internal:/export/team", CachePath: "/data/cache", CacheMaxBytes: 100 << 30}, nil)
	if err != nil {
		t.Fatal(err)
	}
	usage := []proto.PathUsage{
		{Kind: "models", Path: "/data/models", TotalBytes: 1000, AvailableBytes: 900},
		{Kind: proto.PathKindStorage, StorageID: cache.ID, Path: "/mnt/gpuworks/team-cache", TotalBytes: 500 << 30, AvailableBytes: 200 << 30},
		{Kind: proto.PathKindStorageCache, StorageID: cache.ID, Path: "/data/cache", TotalBytes: 2 << 40, AvailableBytes: 1 << 40},
	}
	if err := st.UpdateNodeStatus(gpu3.UUID, proto.NodeStatus{PathUsage: usage}); err != nil {
		t.Fatal(err)
	}
	if err := st.ApplyStorageStatus(gpu3.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{
		{ID: cache.ID, State: proto.StorageStateReady, Path: "/mnt/gpuworks/team-cache", CachePath: "/data/cache", Writable: true},
	}}); err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Source: "modelscope", SourceID: "Qwen/Qwen3-8B", Name: "Qwen3-8B"})
	if err != nil {
		t.Fatal(err)
	}
	used := time.Date(2026, 10, 3, 8, 0, 0, 0, time.UTC)
	var gpu3Cache *store.ModelFile
	for _, mf := range []store.ModelFile{
		{ModelID: model.ID, StorageID: cache.ID, NodeID: gpu3.ID, Path: "/data/cache/Qwen3-8B", State: proto.MFPresent, SizeBytes: 16, Downloaded: 16, LastUsedAt: &used},
		{ModelID: model.ID, StorageID: cache.ID, Path: "Qwen/Qwen3-8B", State: proto.MFPresent, SizeBytes: 16, Downloaded: 16, Acquisition: proto.ModelAcquireImported},
		{ModelID: model.ID, StorageID: cache.ID, NodeID: gpu2.ID, Path: "/data/cache/Qwen3-8B", State: proto.MFDownloading, SizeBytes: 16, Downloaded: 4},
	} {
		mf := mf
		row, err := st.UpsertModelFile(&mf)
		if err != nil {
			t.Fatal(err)
		}
		if mf.NodeID == gpu3.ID {
			gpu3Cache = row
		}
	}
	// The deployment's instance on gpu3 reads gpu3's cache entry; that row,
	// not the storage's shared copy, lists it.
	dep, err := st.CreateDeployment(&store.Deployment{Name: "q8", ServedName: "q8", ModelID: model.ID, SaveToStorageID: cache.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateInstances([]*store.Instance{{UUID: "q8-0", DeploymentID: dep.ID, NodeID: gpu3.ID, State: proto.InstRunning}},
		[][]store.InstanceModelFile{{{ModelID: model.ID, ModelFileID: gpu3Cache.ID, HostPath: gpu3Cache.Path}}}); err != nil {
		t.Fatal(err)
	}

	code, body := get(fmt.Sprintf("/api/v1/storages/%d/model-files", cache.ID))
	if code != http.StatusOK {
		t.Fatalf("model files = %d %s", code, body)
	}
	var files []dto.StorageModelFile
	if err := unmarshalList([]byte(body), &files); err != nil || len(files) != 3 {
		t.Fatalf("model files = %s (%v)", body, err)
	}
	if files[0].NodeUUID != "" || files[0].Cache || files[0].Path != "Qwen/Qwen3-8B" || files[0].ModelName != "Qwen3-8B" {
		t.Fatalf("first row should be the shared copy: %+v", files[0])
	}
	if files[1].NodeName != "gpu2" || !files[1].Cache || files[1].Progress != 0.25 || files[2].NodeName != "gpu3" || files[2].LastUsedAt == nil || !files[2].LastUsedAt.Equal(used) {
		t.Fatalf("node cache rows = %+v / %+v", files[1], files[2])
	}
	if fmt.Sprint(files[2].DeploymentIDs) != fmt.Sprint([]uint{dep.ID}) || len(files[0].DeploymentIDs) != 0 || len(files[1].DeploymentIDs) != 0 {
		t.Fatalf("deployment ids = %v / %v / %v", files[0].DeploymentIDs, files[1].DeploymentIDs, files[2].DeploymentIDs)
	}
	for _, key := range []string{`"model_name":"Qwen3-8B"`, `"cache":true`, `"last_used_at":"2026-10-03T08:00:00Z"`, `"deployment_ids":[`} {
		if !strings.Contains(body, key) {
			t.Fatalf("wire JSON lacks %s: %s", key, body)
		}
	}
	if code, _ := get("/api/v1/storages/99/model-files"); code != http.StatusNotFound {
		t.Fatalf("unknown storage = %d", code)
	}

	code, body = get(fmt.Sprintf("/api/v1/storages/%d?all_nodes=1", cache.ID))
	if code != http.StatusOK {
		t.Fatalf("storage = %d %s", code, body)
	}
	var detail dto.Storage
	if err := json.Unmarshal([]byte(body), &detail); err != nil {
		t.Fatal(err)
	}
	byName := map[string]dto.StorageNodeStatus{}
	for _, n := range detail.Nodes.All {
		byName[n.NodeName] = n
	}
	if g := byName["gpu3"]; g.TotalBytes != 500<<30 || g.AvailableBytes != 200<<30 || g.CacheTotalBytes != 2<<40 || g.CacheAvailableBytes != 1<<40 {
		t.Fatalf("gpu3 capacity = %+v", g)
	}
	if g := byName["gpu2"]; g.TotalBytes != 0 || g.CacheTotalBytes != 0 {
		t.Fatalf("gpu2 reported nothing yet: %+v", g)
	}
}
