package cloud

import (
	"net/http"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/resourceclaim"
	cloudstore "gpuworks.ai/gpuworks/internal/server/store/zstack/cloud"
)

func TestGPUDescriptionLimitRejectsReservationWithoutChangingCloud(t *testing.T) {
	_, svc, fixture := newCloudRuntimeFixture(t)
	fixture.description.Store(strings.Repeat("原", 2048))
	err := svc.BeforeCreate(t.Context(), provider.InferenceNodeCreateRequest{NodeUUID: "cloud-node", InstanceUUID: "too-long", GPUIndexes: []int{0}})
	if err == nil || !strings.Contains(err.Error(), "description") || fixture.writes.Load() != 0 || fixture.gpu.Load() {
		t.Fatalf("oversized description must fail before a Cloud mutation: err=%v writes=%d", err, fixture.writes.Load())
	}
}

func TestGPUOwnershipRequiresAnEnrolledServerAddress(t *testing.T) {
	st, svc, fixture := newCloudRuntimeFixture(t)
	if err := st.SetNodeServerURL("cloud-node", ""); err != nil {
		t.Fatal(err)
	}
	err := svc.BeforeCreate(t.Context(), provider.InferenceNodeCreateRequest{NodeUUID: "cloud-node", InstanceUUID: "no-address", GPUIndexes: []int{0}})
	if err == nil || fixture.writes.Load() != 0 {
		t.Fatalf("missing server address must not produce an anonymous reservation: %v", err)
	}
}

func TestInterruptedGPUReservationRequiresStateAndDescription(t *testing.T) {
	st, svc, fixture := newCloudRuntimeFixture(t)
	fixture.reserveFailureStatus.Store(http.StatusGatewayTimeout)
	fixture.reserveBeforeTimeout.Store(true)
	request := provider.InferenceNodeCreateRequest{NodeUUID: "cloud-node", InstanceUUID: "interrupted-description", GPUIndexes: []int{0}}
	if err := svc.BeforeCreate(t.Context(), request); err == nil {
		t.Fatal("fixture must interrupt the reservation response")
	}
	ledger := cloudstore.New(st.DB())
	service := resourceclaim.New(st, svc)
	fixture.description.Store("")
	if err := service.ReconcileCloudResourceLeases(t.Context()); err != nil {
		t.Fatal(err)
	}
	lease, err := ledger.CloudResourceLeaseByOperation("interrupted-description/0")
	if err != nil || lease.State != cloudstore.CloudLeaseReservePending {
		t.Fatalf("disabled without ownership cannot confirm the reservation: %+v err=%v", lease, err)
	}
	fixture.description.Store("被gpuworks-192.0.2.100使用中")
	if err := service.ReconcileCloudResourceLeases(t.Context()); err != nil {
		t.Fatal(err)
	}
	lease, err = ledger.CloudResourceLeaseByOperation("interrupted-description/0")
	if err != nil || lease.State != cloudstore.CloudLeaseReserved || fixture.writes.Load() != 1 {
		t.Fatalf("matching state and ownership must confirm without replaying the write: %+v err=%v", lease, err)
	}
}
