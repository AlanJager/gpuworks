package store

import (
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
)

func TestStorageOutageIsOnTheTimelineOfDeploymentsReadingIt(t *testing.T) {
	f := newStorageFixture(t)
	st := f.st
	onNFS, err := st.CreateDeployment(&Deployment{Name: "q06", ServedName: "q06", State: DeployActive, NodeID: f.n1.ID, Replicas: 1, SaveToStorageID: f.nfs.ID})
	if err != nil {
		t.Fatal(err)
	}
	onLocal, err := st.CreateDeployment(&Deployment{Name: "local", ServedName: "local", State: DeployActive, NodeID: f.n1.ID, Replicas: 1, SaveToStorageID: f.local.ID})
	if err != nil {
		t.Fatal(err)
	}
	elsewhere, err := st.CreateDeployment(&Deployment{Name: "n2", ServedName: "n2", State: DeployActive, NodeID: f.n2.ID, Replicas: 1, SaveToStorageID: f.nfs.ID})
	if err != nil {
		t.Fatal(err)
	}
	stopped, err := st.CreateDeployment(&Deployment{Name: "old", ServedName: "old", State: DeployActive, NodeID: f.n1.ID, Replicas: 1, SaveToStorageID: f.nfs.ID})
	if err != nil {
		t.Fatal(err)
	}
	// Which storage an instance reads is what it recorded at launch, not its
	// deployment's: "local" saves into the NFS but reads a node copy.
	shared, err := st.UpsertModelFile(&ModelFile{ModelID: 1, StorageID: f.nfs.ID, Path: "Qwen/Qwen3-0.6B", State: proto.MFPresent})
	if err != nil {
		t.Fatal(err)
	}
	own, err := st.UpsertModelFile(&ModelFile{ModelID: 1, StorageID: f.local.ID, NodeID: f.n1.ID, Path: "/opt/gpuworks/models/q", State: proto.MFPresent})
	if err != nil {
		t.Fatal(err)
	}
	onLocal.SaveToStorageID = f.nfs.ID
	if err := st.DB().Save(onLocal).Error; err != nil {
		t.Fatal(err)
	}
	readsShared := []InstanceModelFile{{ModelID: 1, ModelFileID: shared.ID, HostPath: "/mnt/gpuworks/model-archive/Qwen/Qwen3-0.6B"}}
	if err := st.CreateInstances([]*Instance{
		{UUID: "q06-0", DeploymentID: onNFS.ID, NodeID: f.n1.ID, State: proto.InstStarting},
		{UUID: "local-0", DeploymentID: onLocal.ID, NodeID: f.n1.ID, State: proto.InstRunning},
		{UUID: "n2-0", DeploymentID: elsewhere.ID, NodeID: f.n2.ID, State: proto.InstRunning},
		{UUID: "old-0", DeploymentID: stopped.ID, NodeID: f.n1.ID, State: proto.InstStopped},
	}, [][]InstanceModelFile{readsShared, {{ModelID: 1, ModelFileID: own.ID, HostPath: own.Path}}, readsShared, readsShared}); err != nil {
		t.Fatal(err)
	}
	codes := func(dep uint) string {
		t.Helper()
		events, err := st.DeploymentEvents(dep, 20)
		if err != nil {
			t.Fatal(err)
		}
		var out []string
		for _, ev := range events { // oldest first
			if strings.HasPrefix(ev.Code, "STORAGE_") {
				out = append(out, ev.Code)
			}
		}
		return strings.Join(out, ",")
	}
	ready := proto.StorageStatus{ID: f.nfs.ID, State: proto.StorageStateReady, Path: "/mnt/gpuworks/model-archive", Writable: true}
	down := proto.StorageStatus{ID: f.nfs.ID, State: proto.StorageStateUnresponsive, Path: "/mnt/gpuworks/model-archive", Message: "statfs: filesystem did not respond"}

	f.report(t, f.n1, ready) // the first report is not a transition
	f.report(t, f.n1, down)
	f.report(t, f.n1, down) // unchanged: no second event
	f.report(t, f.n1, ready)
	if got := codes(onNFS.ID); got != EventStorageUnavailable+","+EventStorageRecovered {
		t.Fatalf("timeline = %q", got)
	}
	for _, dep := range []*Deployment{onLocal, elsewhere, stopped} {
		if got := codes(dep.ID); got != "" {
			t.Fatalf("deployment %s got storage events %q", dep.Name, got)
		}
	}
	events, _ := st.DeploymentEvents(onNFS.ID, 20)
	for _, ev := range events {
		if ev.Code == EventStorageUnavailable && (!strings.Contains(ev.Data, `"state":"unresponsive"`) || !strings.Contains(ev.Data, `"storage":"model-archive"`) || !strings.Contains(ev.Data, `"node":"n1"`)) {
			t.Fatalf("unavailable event params = %s", ev.Data)
		}
	}
	// A node whose first report already finds the storage down says so.
	f.report(t, f.n2, down)
	if got := codes(elsewhere.ID); got != EventStorageUnavailable {
		t.Fatalf("first report down: %q", got)
	}
}
