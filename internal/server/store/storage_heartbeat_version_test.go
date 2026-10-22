package store

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/version"
)

// The heartbeat a node writes names this control plane and its build version;
// the node must not stamp its own.
func TestStorageTaskHeartbeatCarriesTheServersVersion(t *testing.T) {
	old := version.Version
	version.Version = "v9.9.9-test"
	t.Cleanup(func() { version.Version = old })

	f := newStorageFixture(t)
	if err := f.st.DB().Model(&Node{}).Where("id = ?", f.n1.ID).Update("state", NodeStateOnline).Error; err != nil {
		t.Fatal(err)
	}
	f.report(t, f.n1, proto.StorageStatus{ID: f.nfs.ID, State: proto.StorageStateReady, Path: "/m", Writable: true})
	tasks, err := f.st.StorageTasksForNode(f.n1)
	if err != nil {
		t.Fatal(err)
	}
	var hb *proto.StorageHeartbeat
	for _, task := range tasks {
		if task.ID == f.nfs.ID {
			hb = task.Heartbeat
		}
	}
	if hb == nil {
		t.Fatalf("the heartbeat node got no heartbeat: %+v", tasks)
	}
	identity := f.st.ControlPlaneIdentity()
	if hb.Version != "v9.9.9-test" || hb.ControlPlaneID != identity.ID || hb.IntervalSeconds != int(StorageHeartbeatInterval.Seconds()) {
		t.Fatalf("heartbeat = %+v", hb)
	}
}
