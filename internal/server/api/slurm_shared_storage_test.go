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

// sharingDriver is a cloud whose HPC clusters share a directory across their
// nodes, as E-HPC does under /opt (provider.HPCSharedStorage).
type sharingDriver struct{ *applyDriver }

func (sharingDriver) Name() string              { return "api-slurm-sharing-provider" }
func (sharingDriver) HPCSharedModelDir() string { return "/opt/gpuworks-models" }

// enrollFirstNode enrolls compute001 with the rig's only entry.
func enrollFirstNode(t *testing.T, rig *tokenRig) {
	t.Helper()
	token := regexp.MustCompile(`token=([0-9a-f]+)`).FindStringSubmatch(slurmEntryItem(t, rig)["setup_command"].(string))[1]
	tok, _ := json.Marshal(token)
	if rec := rig.do("", http.MethodPost, "/api/v1/slurm/enroll", `{"token":`+string(tok)+`,"slurm_cluster_name":"ehpc-1","node_name":"compute001"}`); rec.Code != http.StatusOK {
		t.Fatalf("enroll = %d %s", rec.Code, rec.Body)
	}
}

// An entry made from a cloud cluster whose driver knows a shared directory
// (E-HPC) is offered to register it as model storage once a node has
// joined, with what the add-storage form is filled with; the offer goes once
// a node of it has a shared storage. A cloud that knows none offers nothing.
func TestAnEHPCEntryIsOfferedToRegisterItsNASAsModelStorage(t *testing.T) {
	drv := &applyDriver{answer: okRun("")}
	ar := newApplyRigFor(t, drv, sharingDriver{drv})
	rig := ar.tokenRig
	uuid, _ := ar.create("ehpc-sz-1")
	hint := func() any {
		t.Helper()
		item := slurmEntryItem(t, rig)
		return item["shared_storage_hint"]
	}
	if h := hint(); h != nil {
		t.Fatalf("offered before any node joined: %v", h)
	}
	enrollFirstNode(t, rig)
	want := map[string]any{"name": "ehpc-ehpc-sz-1-models", "type": "shared_dir", "path": "/opt/gpuworks-models", "label_key": "gpuworks.ai/slurm-cluster", "label_value": uuid}
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

func TestACloudThatKnowsNoSharedDirectoryOffersNoStorage(t *testing.T) {
	rig := newApplyRig(t, okRun(""))
	rig.create("ehpc-sz-1")
	enrollFirstNode(t, rig.tokenRig)
	if h := slurmEntryItem(t, rig.tokenRig)["shared_storage_hint"]; h != nil {
		t.Fatalf("offered on a cloud that names no shared directory: %v", h)
	}
}
