package resourceclaim

import (
	"context"
	"fmt"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/inventory"
	cloudstore "gpuworks.ai/gpuworks/internal/server/store/zstack/cloud"
)

func (s *Service) currentCloudHost(ctx context.Context, host *cloudstore.CloudHost, gpuIDs []string) (inventory.CloudResource, error) {
	driver, creds, err := s.driverCreds(host.ProviderID)
	if err != nil {
		return inventory.CloudResource{}, err
	}
	querier, ok := driver.(HostResourceQuerier)
	if !ok {
		return inventory.CloudResource{}, provider.ErrUnsupported
	}
	return querier.QueryHostResources(ctx, creds, host.ExternalID, gpuIDs)
}

// The caller holds resourceMu from observation through confirmed mutation.
// Existing reserved holders permit dGPU sharing. Only the matching operation
// may resume a pending reservation; other unresolved holders remain protected.
func (s *Service) gpuReservationShared(host *cloudstore.CloudHost, resource inventory.CloudResource, resourceID, instanceUUID, operationID string) (bool, error) {
	var gpu *inventory.CloudGPU
	for i := range resource.GPUs {
		if resource.GPUs[i].ExternalID == resourceID {
			gpu = &resource.GPUs[i]
			break
		}
	}
	if gpu == nil || !strings.EqualFold(gpu.Allocation, "Unallocated") || gpu.VMUUID != "" {
		return false, fmt.Errorf("Cloud GPU %s is not available", resourceID)
	}
	holders, err := s.cloudStore.ActiveCloudResourceLeasesForResource(host.ProviderID, "gpu", resourceID)
	if err != nil {
		return false, err
	}
	if len(holders) == 0 {
		if !isAllocatableGPU(*gpu) {
			return false, fmt.Errorf("Cloud GPU %s is not available", resourceID)
		}
		return false, nil
	}
	pending := false
	for _, holder := range holders {
		if holder.CloudHostID != host.ID || holder.NodeUUID != host.NodeUUID {
			return false, cloudstore.ErrCloudConflict
		}
		if holder.IsReservationPending() && holder.InstanceUUID == instanceUUID && holder.OperationID == operationID {
			pending = true
			continue
		}
		if holder.State != cloudstore.CloudLeaseReserved {
			return false, fmt.Errorf("Cloud GPU %s has an unresolved resource operation", resourceID)
		}
	}
	if strings.EqualFold(gpu.State, "Disabled") {
		return true, nil
	}
	if pending && len(holders) == 1 && isAllocatableGPU(*gpu) {
		return false, nil
	}
	return false, fmt.Errorf("Cloud GPU %s no longer matches its resource leases", resourceID)
}

// isAllocatableGPU is used when a deployment requests a concrete device. It
// must not decide host onboarding: inventory also includes occupied GPUs.
func isAllocatableGPU(gpu inventory.CloudGPU) bool {
	return strings.EqualFold(gpu.State, "Enabled") &&
		strings.EqualFold(gpu.Allocation, "Unallocated")
}
