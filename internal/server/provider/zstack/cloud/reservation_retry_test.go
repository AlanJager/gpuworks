package cloud

import (
	"errors"
	"fmt"
	"net/http"
	"strings"
	"testing"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/apiclient"
	cloudstore "gpuworks.ai/gpuworks/internal/server/store/zstack/cloud"
)

func TestCloudRejectedReservationDoesNotBlockNextAttempt(t *testing.T) {
	for _, status := range []int{http.StatusBadRequest, http.StatusServiceUnavailable} {
		for _, retryInstance := range []string{"inference", "another-instance"} {
			t.Run(fmt.Sprintf("HTTP%d/%s", status, retryInstance), func(t *testing.T) {
				st, svc, fixture := newCloudRuntimeFixture(t)
				ledger := cloudstore.New(st.DB())
				req := provider.InferenceNodeCreateRequest{NodeUUID: "cloud-node", InstanceUUID: "inference", GPUIndexes: []int{0}}
				fixture.reserveFailureStatus.Store(int32(status))
				var apiErr *apiclient.APIError
				if err := svc.BeforeCreate(t.Context(), req); !errors.As(err, &apiErr) || apiErr.StatusCode != status || apiErr.OutcomeUnknown {
					t.Fatalf("reservation must return the definitive Cloud failure: %v", err)
				}
				if _, err := ledger.CloudResourceLeaseByOperation("inference/0"); !errors.Is(err, cloudstore.ErrNotFound) {
					t.Fatalf("rejected reservation was not deleted: %v", err)
				}
				if err := svc.OnCreateFailed(t.Context(), req); err != nil {
					t.Fatal(err)
				}
				if fixture.gpu.Load() || fixture.writes.Load() != 1 {
					t.Fatal("rejected reservation must not hold or release the GPU")
				}
				fixture.reserveFailureStatus.Store(0)
				req.InstanceUUID = retryInstance
				if err := svc.BeforeCreate(t.Context(), req); err != nil {
					t.Fatalf("rejected reservation blocked the next attempt: %v", err)
				}
				leases, err := ledger.CloudResourceLeasesForInstance(req.InstanceUUID)
				if err != nil || len(leases) != 1 || leases[0].State != cloudstore.CloudLeaseReserved || !fixture.gpu.Load() || fixture.writes.Load() != 2 {
					t.Fatalf("next attempt did not reserve the GPU: leases=%+v err=%v", leases, err)
				}
			})
		}
	}
}

func TestCloudPendingReservationCanResumeOnlyItsOwnOperation(t *testing.T) {
	for _, state := range []string{cloudstore.CloudLeaseReservePending, cloudstore.CloudLeaseUnknown} {
		for _, applied := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/applied=%v", state, applied), func(t *testing.T) {
				st, svc, fixture := newCloudRuntimeFixture(t)
				ledger := cloudstore.New(st.DB())
				req := provider.InferenceNodeCreateRequest{NodeUUID: "cloud-node", InstanceUUID: "inference", GPUIndexes: []int{0}}
				fixture.reserveFailureStatus.Store(http.StatusGatewayTimeout)
				fixture.reserveBeforeTimeout.Store(applied)
				var apiErr *apiclient.APIError
				if err := svc.BeforeCreate(t.Context(), req); !errors.As(err, &apiErr) || !apiErr.OutcomeUnknown {
					t.Fatalf("gateway timeout must leave the reservation unconfirmed: %v", err)
				}
				lease, err := ledger.CloudResourceLeaseByOperation("inference/0")
				if err != nil || lease.State != cloudstore.CloudLeaseReservePending {
					t.Fatalf("new reservation must use reserve_pending: lease=%+v err=%v", lease, err)
				}
				if state == cloudstore.CloudLeaseUnknown {
					// Reproduce a row written by an older server, without changing new-write defaults.
					if err := st.DB().Model(&cloudstore.CloudResourceLease{}).Where("id = ?", lease.ID).Update("state", state).Error; err != nil {
						t.Fatal(err)
					}
				}
				unresolved, err := ledger.UnresolvedCloudResourceLeases()
				if err != nil || len(unresolved) != 1 || unresolved[0].ID != lease.ID || !unresolved[0].IsReservationPending() {
					t.Fatalf("pending reservation was omitted from recovery: leases=%+v err=%v", unresolved, err)
				}
				fixture.reserveFailureStatus.Store(0)
				other := req
				other.InstanceUUID = "another-instance"
				if err := svc.BeforeCreate(t.Context(), other); err == nil || fixture.writes.Load() != 1 {
					t.Fatalf("another instance bypassed the pending reservation: %v", err)
				}
				if err := svc.BeforeCreate(t.Context(), req); err != nil {
					t.Fatalf("the owning operation could not resume: %v", err)
				}
				resumed, err := ledger.CloudResourceLeaseByOperation("inference/0")
				if err != nil || resumed.ID != lease.ID || resumed.State != cloudstore.CloudLeaseReserved || resumed.ErrorCode != "" {
					t.Fatalf("resume must confirm the existing reservation: lease=%+v err=%v", resumed, err)
				}
				wantWrites := int64(2)
				if applied {
					wantWrites = 1
				}
				if !fixture.gpu.Load() || fixture.writes.Load() != wantWrites {
					t.Fatalf("resume must reserve only if Cloud has not already disabled the GPU: disabled=%v writes=%d", fixture.gpu.Load(), fixture.writes.Load())
				}
				if err := svc.BeforeCreate(t.Context(), req); err != nil || fixture.writes.Load() != wantWrites {
					t.Fatalf("confirmed reservation was submitted twice: %v", err)
				}
				if err := svc.AfterInstanceTerminal(t.Context(), provider.InferenceNodeStopRequest{
					NodeUUID: req.NodeUUID, InstanceUUID: req.InstanceUUID, State: proto.InstStopped,
				}); err != nil {
					t.Fatal(err)
				}
				if fixture.gpu.Load() || fixture.writes.Load() != wantWrites+1 {
					t.Fatal("resumed reservation did not release normally")
				}
			})
		}
	}
}

func TestCloudRejectedRetryPreservesEarlierUnconfirmedReservation(t *testing.T) {
	st, svc, fixture := newCloudRuntimeFixture(t)
	ledger := cloudstore.New(st.DB())
	req := provider.InferenceNodeCreateRequest{NodeUUID: "cloud-node", InstanceUUID: "inference", GPUIndexes: []int{0}}
	fixture.reserveFailureStatus.Store(http.StatusGatewayTimeout)
	if err := svc.BeforeCreate(t.Context(), req); err == nil {
		t.Fatal("gateway timeout was ignored")
	}
	fixture.reserveFailureStatus.Store(http.StatusBadRequest)
	var apiErr *apiclient.APIError
	if err := svc.BeforeCreate(t.Context(), req); !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest {
		t.Fatalf("pending operation did not reach Cloud again: %v", err)
	}
	lease, err := ledger.CloudResourceLeaseByOperation("inference/0")
	if err != nil || lease.State != cloudstore.CloudLeaseReservePending || fixture.writes.Load() != 2 {
		t.Fatalf("rejecting the retry must not erase the earlier uncertain write: lease=%+v err=%v", lease, err)
	}
	fixture.reserveFailureStatus.Store(0)
	if err := svc.BeforeCreate(t.Context(), req); err != nil {
		t.Fatalf("pending reservation could not retry after rejection: %v", err)
	}
}

func TestCloudPendingReservationStillChecksLiveAllocationAndIdentity(t *testing.T) {
	for _, conflict := range []string{"allocated", "different-instance", "different-operation", "release-pending"} {
		t.Run(conflict, func(t *testing.T) {
			st, svc, fixture := newCloudRuntimeFixture(t)
			ledger := cloudstore.New(st.DB())
			req := provider.InferenceNodeCreateRequest{NodeUUID: "cloud-node", InstanceUUID: "inference", GPUIndexes: []int{0}}
			fixture.reserveFailureStatus.Store(http.StatusGatewayTimeout)
			if err := svc.BeforeCreate(t.Context(), req); err == nil {
				t.Fatal("gateway timeout was ignored")
			}
			lease, err := ledger.CloudResourceLeaseByOperation("inference/0")
			if err != nil {
				t.Fatal(err)
			}
			switch conflict {
			case "allocated":
				fixture.allocated.Store(true)
			case "different-instance":
				if err := st.DB().Model(&cloudstore.CloudResourceLease{}).Where("id = ?", lease.ID).Update("instance_uuid", "another-instance").Error; err != nil {
					t.Fatal(err)
				}
			case "different-operation":
				if err := st.DB().Model(&cloudstore.CloudResourceLease{}).Where("id = ?", lease.ID).Update("operation_id", "another-operation/0").Error; err != nil {
					t.Fatal(err)
				}
			case "release-pending":
				if err := ledger.SetCloudResourceLeaseState(lease.ID, cloudstore.CloudLeaseReleasePending, ""); err != nil {
					t.Fatal(err)
				}
			}
			fixture.reserveFailureStatus.Store(0)
			if err := svc.BeforeCreate(t.Context(), req); err == nil || fixture.writes.Load() != 1 {
				t.Fatalf("pending retry bypassed %s protection: %v", conflict, err)
			}
			if _, err := ledger.CloudResourceLeaseByID(lease.ID); err != nil {
				t.Fatalf("rejected retry lost the protected reservation: %v", err)
			}
		})
	}
}

func TestCloudReservationPropagatesPendingRecordDeletionFailure(t *testing.T) {
	st, svc, fixture := newCloudRuntimeFixture(t)
	if err := st.DB().Exec(`CREATE TRIGGER reject_lease_delete BEFORE DELETE ON cloud_resource_leases
		BEGIN SELECT RAISE(ABORT, 'injected lease deletion failure'); END`).Error; err != nil {
		t.Fatal(err)
	}
	fixture.reserveFailureStatus.Store(http.StatusBadRequest)
	req := provider.InferenceNodeCreateRequest{NodeUUID: "cloud-node", InstanceUUID: "inference", GPUIndexes: []int{0}}
	err := svc.BeforeCreate(t.Context(), req)
	var apiErr *apiclient.APIError
	if !errors.As(err, &apiErr) || apiErr.StatusCode != http.StatusBadRequest || !strings.Contains(err.Error(), "injected lease deletion failure") {
		t.Fatalf("reservation must preserve both Cloud and deletion errors: %v", err)
	}
	lease, err := cloudstore.New(st.DB()).CloudResourceLeaseByOperation("inference/0")
	if err != nil || lease.State != cloudstore.CloudLeaseReservePending {
		t.Fatalf("failed deletion must leave the pending record: lease=%+v err=%v", lease, err)
	}
}
