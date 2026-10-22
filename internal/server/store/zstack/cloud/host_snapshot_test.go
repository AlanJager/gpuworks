package cloud_test

import (
	"testing"

	cloudstore "gpuworks.ai/gpuworks/internal/server/store/zstack/cloud"
)

func TestCloudHostSnapshotPreservesReservationBaselineOnReinstall(t *testing.T) {
	s := newTestStore(t)
	first := cloudstore.CloudHost{ProviderID: 1, ExternalID: "host", NodeUUID: "first-node", Snapshot: `{"gpus":[]}`, InitialCPUReserved: int64p(2), InitialMemoryReserved: int64p(4 << 30)}
	if err := cloudstore.New(s.DB()).SaveCloudHostSnapshot(first); err != nil {
		t.Fatal(err)
	}
	next := first
	next.NodeUUID, next.Snapshot = "replacement-node", `{"gpus":[{"external_id":"gpu"}]}`
	next.InitialCPUReserved, next.InitialMemoryReserved = int64p(4), int64p(8<<30)
	if err := cloudstore.New(s.DB()).SaveCloudHostSnapshot(next); err != nil {
		t.Fatal(err)
	}
	hosts, err := cloudstore.New(s.DB()).CloudHosts(1)
	if err != nil || len(hosts) != 1 {
		t.Fatalf("hosts=%+v err=%v", hosts, err)
	}
	host := hosts[0]
	if host.NodeUUID != next.NodeUUID || host.Snapshot != next.Snapshot || *host.InitialCPUReserved != 2 || *host.InitialMemoryReserved != 4<<30 {
		t.Fatalf("reinstall replaced the resource baseline or kept a stale node: %+v", host)
	}
}
