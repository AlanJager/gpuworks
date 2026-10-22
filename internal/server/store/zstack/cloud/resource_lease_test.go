package cloud_test

import (
	"errors"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
	cloudstore "gpuworks.ai/gpuworks/internal/server/store/zstack/cloud"
)

func TestCloudResourceLeaseIsIdempotentAndTracksGPURelease(t *testing.T) {
	s := newTestStore(t)
	provider, err := s.CreateProvider(&store.Provider{Name: "cloud", Kind: "zstack", CredsCipher: "cipher"})
	if err != nil {
		t.Fatal(err)
	}
	host := &cloudstore.CloudHost{ProviderID: provider.ID, ExternalID: "host-1"}
	if err := s.DB().Create(host).Error; err != nil {
		t.Fatal(err)
	}
	lease := cloudstore.CloudResourceLease{OperationID: "op-1", ProviderID: provider.ID, CloudHostID: host.ID, NodeUUID: "node-1", InstanceUUID: "instance-1", ResourceType: "gpu", ResourceID: "gpu-1", Amount: 1, Unit: "device", State: cloudstore.CloudLeaseReserved}
	created, err := cloudstore.New(s.DB()).CreateCloudResourceLease(&lease)
	if err != nil {
		t.Fatal(err)
	}
	replayed, err := cloudstore.New(s.DB()).CreateCloudResourceLease(&lease)
	if err != nil || replayed.ID != created.ID {
		t.Fatalf("idempotent replay = %+v, err=%v", replayed, err)
	}
	conflicting := lease
	conflicting.ResourceID = "gpu-2"
	if _, err := cloudstore.New(s.DB()).CreateCloudResourceLease(&conflicting); !errors.Is(err, cloudstore.ErrCloudConflict) {
		t.Fatalf("different GPU reused the operation: %v", err)
	}
	if leases, err := cloudstore.New(s.DB()).CloudResourceLeasesForInstance(lease.InstanceUUID); err != nil || len(leases) != 1 || leases[0].ResourceID != "gpu-1" {
		t.Fatalf("owned GPUs = %+v, err=%v", leases, err)
	}
	if err := cloudstore.New(s.DB()).SetCloudResourceLeaseState(created.ID, cloudstore.CloudLeaseReleased, ""); err != nil {
		t.Fatal(err)
	}
	if leases, err := cloudstore.New(s.DB()).CloudResourceLeasesForInstance(lease.InstanceUUID); err != nil || len(leases) != 0 {
		t.Fatalf("released GPU still held: %+v, err=%v", leases, err)
	}
}

func TestCloudResourceLeaseDeletionOnlyAcceptsPendingReservations(t *testing.T) {
	s := newTestStore(t)
	ledger := cloudstore.New(s.DB())
	provider, err := s.CreateProvider(&store.Provider{Name: "cloud", Kind: "zstack", CredsCipher: "cipher"})
	if err != nil {
		t.Fatal(err)
	}
	host := cloudstore.CloudHost{ProviderID: provider.ID, ExternalID: "host-1"}
	if err := s.DB().Create(&host).Error; err != nil {
		t.Fatal(err)
	}
	lease, err := ledger.CreateCloudResourceLease(&cloudstore.CloudResourceLease{
		OperationID: "pending", ProviderID: host.ProviderID, CloudHostID: host.ID,
		ResourceType: "gpu", ResourceID: "gpu-1", Amount: 1, Unit: "device",
	})
	if err != nil || lease.State != cloudstore.CloudLeaseReservePending {
		t.Fatalf("default reservation state = %+v, err=%v", lease, err)
	}
	if err := ledger.DeletePendingCloudResourceLease(0); err == nil {
		t.Fatal("deletion accepted an empty lease ID")
	}
	if err := ledger.SetCloudResourceLeaseState(lease.ID, cloudstore.CloudLeaseReserved, ""); err != nil {
		t.Fatal(err)
	}
	if err := ledger.DeletePendingCloudResourceLease(lease.ID); !errors.Is(err, cloudstore.ErrCloudConflict) {
		t.Fatalf("confirmed ownership must not be deleted as a failed reservation: %v", err)
	}
	persisted, err := ledger.CloudResourceLeaseByID(lease.ID)
	if err != nil || persisted.State != cloudstore.CloudLeaseReserved {
		t.Fatalf("rejected deletion changed confirmed ownership: lease=%+v err=%v", persisted, err)
	}
	if err := ledger.SetCloudResourceLeaseState(lease.ID, cloudstore.CloudLeaseReservePending, ""); err != nil {
		t.Fatal(err)
	}
	if err := ledger.DeletePendingCloudResourceLease(lease.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := ledger.CloudResourceLeaseByID(lease.ID); !errors.Is(err, cloudstore.ErrNotFound) {
		t.Fatalf("pending reservation was not deleted: %v", err)
	}
}

func int64p(v int64) *int64 { return &v }

func newTestStore(t *testing.T) *store.Store {
	t.Helper()
	s, err := store.Open(":memory:", bus.New())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = s.Close() })
	return s
}
