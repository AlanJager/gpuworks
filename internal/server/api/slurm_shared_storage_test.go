package api

import (
	"encoding/json"
	"net/http"
	"reflect"
	"regexp"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// An entry made from an E-HPC cluster is offered to register the cluster's
// NAS as model storage once a node has joined, with what the add-storage form
// is filled with; the offer goes once a node of it has a shared storage.
func TestAnEHPCEntryIsOfferedToRegisterItsNASAsModelStorage(t *testing.T) {
	rig := newTokenRig(t)
	acct := &store.Provider{Name: "ali", Kind: "aliyun"}
	if err := rig.st.DB().Create(acct).Error; err != nil {
		t.Fatal(err)
	}
	rec := rig.do(rig.session, http.MethodPost, "/api/v1/slurm/clusters", `{"name":"GPU Lab","server_url":"https://gw","provider_id":`+jsonUint(acct.ID)+`,"provider_cluster_id":"ehpc-1"}`)
	if rec.Code != http.StatusCreated {
		t.Fatalf("create = %d %s", rec.Code, rec.Body)
	}
	uuid := decodeMap(t, rec.Body.Bytes())["uuid"].(string)
	hint := func() any {
		t.Helper()
		item := slurmEntryItem(t, rig)
		return item["shared_storage_hint"]
	}
	if h := hint(); h != nil {
		t.Fatalf("offered before any node joined: %v", h)
	}
	token := regexp.MustCompile(`token=([0-9a-f]+)`).FindStringSubmatch(slurmEntryItem(t, rig)["setup_command"].(string))[1]
	tok, _ := json.Marshal(token)
	if rec := rig.do("", http.MethodPost, "/api/v1/slurm/enroll", `{"token":`+string(tok)+`,"slurm_cluster_name":"ehpc-1","node_name":"compute001"}`); rec.Code != http.StatusOK {
		t.Fatalf("enroll = %d %s", rec.Code, rec.Body)
	}
	want := map[string]any{"name": "gpu-lab-models", "type": "shared_dir", "path": "/opt/gpuworks-models", "label_key": "gpuworks.ai/slurm-cluster", "label_value": uuid}
	if h := hint(); !reflect.DeepEqual(h, want) {
		t.Fatalf("hint = %v; want %v", h, want)
	}
	// The node gets a shared storage: no more offer. A local one does not count.
	var node store.Node
	if err := rig.st.DB().Where("slurm_node = ?", "compute001").First(&node).Error; err != nil {
		t.Fatal(err)
	}
	report := []proto.StorageStatus{{ID: rig.st.DefaultLocalStorageID(), State: proto.StorageStateReady, Path: "/opt/gpuworks/models"}}
	if err := rig.st.ApplyStorageStatus(node.ID, proto.StorageStatusReport{Storages: report}); err != nil {
		t.Fatal(err)
	}
	if h := hint(); h == nil {
		t.Fatal("a local storage withdrew the offer")
	}
	nas, err := rig.st.CreateStorage(&store.Storage{Name: "nas", Type: proto.StorageSharedDir, Enabled: true, AllNodes: true, Path: "/opt/gpuworks-models"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	report = append(report, proto.StorageStatus{ID: nas.ID, State: proto.StorageStateReady, Path: "/opt/gpuworks-models"})
	if err := rig.st.ApplyStorageStatus(node.ID, proto.StorageStatusReport{Storages: report}); err != nil {
		t.Fatal(err)
	}
	if h := hint(); h != nil {
		t.Fatalf("still offered with a shared storage: %v", h)
	}
}
