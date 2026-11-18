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

// A node whose local storage directory sits on a shared filesystem (an E-HPC
// node's /opt) is counted in the list and named, with the filesystem, in the
// storage's nodes; a node on a local disk is neither. A shared storage's
// nodes are never flagged: sharing is what it is for.
func TestStorageSaysWhichNodesHaveItsLocalDirectoryOnASharedFilesystem(t *testing.T) {
	st, err := store.Open("file:"+t.Name()+"?mode=memory&cache=shared", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = st.Close() })
	onNAS, err := st.RegisterNode("compute001", store.HashToken("a"), "compute001")
	if err != nil {
		t.Fatal(err)
	}
	onDisk, err := st.RegisterNode("gpu7", store.HashToken("b"), "gpu7")
	if err != nil {
		t.Fatal(err)
	}
	for _, n := range []*store.Node{onNAS, onDisk} {
		if err := st.SetNodeState(n.UUID, store.NodeStateOnline); err != nil {
			t.Fatal(err)
		}
	}
	local := st.DefaultLocalStorageID()
	shared, err := st.CreateStorage(&store.Storage{Name: "nas", Type: proto.StorageSharedDir, Enabled: true, AllNodes: true, Path: "/opt/models"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	report := func(n *store.Node, fsType, source string) {
		t.Helper()
		if err := st.ApplyStorageStatus(n.ID, proto.StorageStatusReport{Storages: []proto.StorageStatus{
			{ID: local, State: proto.StorageStateReady, Path: "/opt/gpuworks/models", Writable: true, SharedFSType: fsType, SharedFSSource: source},
			{ID: shared.ID, State: proto.StorageStateReady, Path: "/opt/models", Writable: true, SharedFSType: "nfs4", SharedFSSource: source},
		}}); err != nil {
			t.Fatal(err)
		}
	}
	report(onNAS, "nfs4", "10.0.0.5:/opt")
	report(onDisk, "", "")
	signer, token := roleTestToken(t, st, "member")
	router := Router(Deps{Store: st, AuthSigner: signer})
	get := func(path string) string {
		t.Helper()
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+token)
		rec := httptest.NewRecorder()
		router.ServeHTTP(rec, req)
		if rec.Code != http.StatusOK {
			t.Fatalf("GET %s = %d %s", path, rec.Code, rec.Body)
		}
		return rec.Body.String()
	}
	body := get(fmt.Sprintf("/api/v1/storages/%d?all_nodes=1", local))
	if !strings.Contains(body, `"on_shared_fs":1,`) {
		t.Fatalf("local storage = %s; want one node on a shared filesystem", body)
	}
	if !strings.Contains(body, `"node_name":"compute001"`) || !strings.Contains(body, `"shared_fs":{"type":"nfs4","source":"10.0.0.5:/opt"}`) || strings.Count(body, `"shared_fs":{`) != 1 {
		t.Fatalf("local storage nodes = %s; want compute001 alone named with nfs4 from 10.0.0.5:/opt", body)
	}
	if body := get(fmt.Sprintf("/api/v1/storages/%d?all_nodes=1", shared.ID)); !strings.Contains(body, `"on_shared_fs":0,`) || strings.Contains(body, `"shared_fs":{`) {
		t.Fatalf("a shared storage is flagged: %s", body)
	}
	if list := get("/api/v1/storages"); !strings.Contains(list, `"on_shared_fs":1,`) {
		t.Fatalf("the list does not count the node: %s", list)
	}
}
