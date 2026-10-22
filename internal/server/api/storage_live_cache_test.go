package api

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

func TestStorageNewNodeCacheWhileAnotherNodeIsServing(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	old, err := st.RegisterNode("old", store.HashToken("old-token"), "old")
	if err != nil {
		t.Fatal(err)
	}
	storage, err := st.CreateStorage(&store.Storage{Name: "cached", Type: proto.StorageNFSCache,
		Enabled: true, AllNodes: true, Path: "/mnt/cached", Export: "nfs.example:/models", CachePath: "/cache/default"},
		[]store.StorageNode{{NodeID: old.ID, CachePath: "/cache/old"}})
	if err != nil {
		t.Fatal(err)
	}
	model, err := st.UpsertModel(&store.Model{Source: "local", SourceID: "model", Name: "model"})
	if err != nil {
		t.Fatal(err)
	}
	file, err := st.UpsertModelFile(&store.ModelFile{ModelID: model.ID, StorageID: storage.ID,
		NodeID: old.ID, Path: "/cache/old/model", State: proto.MFPresent})
	if err != nil {
		t.Fatal(err)
	}
	dep, err := st.CreateDeployment(&store.Deployment{Name: "serving", ServedName: "serving", ModelID: model.ID, NodeID: old.ID})
	if err != nil {
		t.Fatal(err)
	}
	if err := st.CreateInstances([]*store.Instance{{UUID: "serving-instance", DeploymentID: dep.ID, NodeID: old.ID, State: proto.InstRunning}},
		[][]store.InstanceModelFile{{{ModelID: model.ID, ModelFileID: file.ID, HostPath: file.Path}}}); err != nil {
		t.Fatal(err)
	}
	added, err := st.RegisterNode("new", store.HashToken("new-token"), "new")
	if err != nil {
		t.Fatal(err)
	}
	signer, token := roleTestToken(t, st, "admin")
	router := Router(Deps{Store: st, AuthSigner: signer})
	// Match buildPatchRequest: unchanged global fields are sent with the targets.
	body := fmt.Sprintf(`{"name":"cached","path":"/mnt/cached","all_nodes":true,"mount_options":"","cache_path":"/cache/default","cache_max_bytes":0,"targets":[{"node_uuid":%q,"cache_path":"/cache/old"},{"node_uuid":%q,"cache_path":"/cache/new"}]}`, old.UUID, added.UUID)
	req := httptest.NewRequest(http.MethodPatch, fmt.Sprintf("/api/v1/storages/%d", storage.ID), strings.NewReader(body))
	req.Header.Set("Authorization", "Bearer "+token)
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	if rec.Code != http.StatusOK {
		t.Fatalf("adding an unused node cache must preserve the running reader: got %d %s", rec.Code, rec.Body.String())
	}
	targets, err := st.StorageNodes(storage.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(targets) != 2 || targets[0].NodeID != old.ID || targets[0].CachePath != "/cache/old" || targets[1].NodeID != added.ID || targets[1].CachePath != "/cache/new" {
		t.Fatalf("targets = %+v", targets)
	}

	for _, tc := range []struct {
		node      *store.Node
		cachePath string
	}{{old, "/cache/old"}, {added, "/cache/new"}} {
		eff, applies, err := st.EffectiveStorageForNode(storage, tc.node)
		if err != nil || !applies || eff.Path != "/mnt/cached" || eff.CachePath != tc.cachePath {
			t.Fatalf("node %s: effective storage = %+v, applies=%v, err=%v", tc.node.UUID, eff, applies, err)
		}
	}
	instance, err := st.InstanceByUUID("serving-instance")
	if err != nil {
		t.Fatal(err)
	}
	reads, err := st.InstanceModelFiles([]uint{instance.ID})
	if err != nil {
		t.Fatal(err)
	}
	if instance.State != proto.InstRunning || len(reads[instance.ID]) != 1 || reads[instance.ID][0].HostPath != "/cache/old/model" {
		t.Fatalf("serving instance changed: %+v reads=%+v", instance, reads)
	}
}
