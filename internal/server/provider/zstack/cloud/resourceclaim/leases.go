package resourceclaim

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strconv"
	"strings"

	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/apiclient"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/inventory"
	"gpuworks.ai/gpuworks/internal/server/store"
	cloudstore "gpuworks.ai/gpuworks/internal/server/store/zstack/cloud"
)

// TODO: Add recovery for reserve_pending/release_pending leases after Cloud timeouts
// or process restarts, including reservations made before an instance is saved.
// No automatic reconciliation is scheduled; controller retries alone do not
// resolve these states. Decide how to trigger observation and resume cleanup.
//
// ReconcileCloudResourceLeases classifies interrupted provider writes by
// reading the provider state. It never retries a write: an unresolved value is
// kept protected until the observed state is unambiguous.
func (s *Service) ReconcileCloudResourceLeases(ctx context.Context) error {
	s.resourceMu.Lock()
	defer s.resourceMu.Unlock()
	leases, err := s.cloudStore.UnresolvedCloudResourceLeases()
	if err != nil {
		return err
	}
	for _, lease := range leases {
		if err := s.reconcileCloudResourceLease(ctx, lease); err != nil {
			continue
		}
	}
	return nil
}

func (s *Service) reconcileCloudResourceLease(ctx context.Context, lease cloudstore.CloudResourceLease) error {
	if lease.ResourceType != "gpu" {
		return provider.ErrUnsupported
	}
	driver, creds, err := s.driverCreds(lease.ProviderID)
	if err != nil {
		return err
	}
	inspector, ok := driver.(ResourceInspector)
	if !ok {
		return provider.ErrUnsupported
	}
	observation, err := inspector.InspectResource(ctx, creds, ResourceMutation{
		ResourceType: lease.ResourceType, ResourceID: lease.ResourceID,
	})
	if err != nil {
		return err
	}
	return s.reconcileGPULease(lease, observation)
}

func (s *Service) reconcileGPULease(lease cloudstore.CloudResourceLease, observation ResourceObservation) error {
	state := strings.ToLower(strings.TrimSpace(observation.State))
	if lease.NodeUUID != "" {
		ownership, err := s.gpuOwnershipDescription(lease.NodeUUID)
		if err != nil {
			return err
		}
		hasOwnership := hasGPUOwnershipDescription(observation.Description, ownership)
		if (state == "disabled" && !hasOwnership) || (state == "enabled" && hasOwnership) {
			return nil
		}
	}
	switch state {
	case "disabled":
		if lease.IsReservationPending() {
			return s.cloudStore.SetCloudResourceLeaseState(lease.ID, cloudstore.CloudLeaseReserved, "confirmed_reserved")
		}
	case "enabled":
		return s.cloudStore.SetCloudResourceLeaseState(lease.ID, cloudstore.CloudLeaseReleased, "confirmed_released")
	}
	return nil
}

func (s *Service) releaseInstanceLeases(ctx context.Context, instanceUUID string) error {
	s.resourceMu.Lock()
	defer s.resourceMu.Unlock()
	leases, err := s.cloudStore.CloudResourceLeasesForInstance(instanceUUID)
	if err != nil {
		return err
	}
	for _, lease := range leases {
		if err := s.releaseCloudResourceLease(ctx, lease.ID); err != nil {
			return err
		}
	}
	return nil
}

// IsCloudNode reports whether onboarding recorded a Cloud host for this node.
// The driver uses it to select the lifecycle handler; lookup failures are errors.
func (s *Service) IsCloudNode(nodeUUID string) (bool, error) {
	_, err := s.cloudStore.CloudHostByNodeUUID(nodeUUID)
	if err == cloudstore.ErrNotFound {
		return false, nil
	}
	return err == nil, err
}

// CloudResourceMutations maps the scheduler's stable GPU indexes to the Cloud
// device identities captured during discovery. Only identity comes from this
// snapshot; ReserveCloudResources reads live allocation state under its lock.
func (s *Service) CloudResourceMutations(nodeUUID string, gpuIndexes []int) ([]ResourceMutation, error) {
	host, err := s.cloudStore.CloudHostByNodeUUID(nodeUUID)
	if err == cloudstore.ErrNotFound {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	var resource inventory.CloudResource
	if err := json.Unmarshal([]byte(host.Snapshot), &resource); err != nil {
		return nil, fmt.Errorf("invalid Cloud host snapshot: %w", err)
	}
	node, err := s.store.NodeWithGPUs(nodeUUID)
	if err != nil {
		return nil, err
	}
	if node.State != store.NodeStateOnline || node.GPUCount == 0 || node.MemTotalKB == 0 {
		return nil, fmt.Errorf("Cloud host is not ready for resource reservation")
	}
	mutations := make([]ResourceMutation, 0, len(gpuIndexes))
	for _, index := range gpuIndexes {
		gpu, ok := node.GPUByIndex(index)
		if !ok {
			return nil, fmt.Errorf("GPU index %d is not present on Cloud node", index)
		}
		var matched *inventory.CloudGPU
		for i := range resource.GPUs {
			if samePCIAddress(gpu.PCIeBDF, resource.GPUs[i].PCIAddress) {
				matched = &resource.GPUs[i]
				break
			}
		}
		if matched == nil || matched.ExternalID == "" {
			return nil, fmt.Errorf("GPU index %d has no stable Cloud device identity", index)
		}
		mutations = append(mutations, ResourceMutation{ResourceType: "gpu", ResourceID: matched.ExternalID, DesiredState: "Disabled"})
	}
	return mutations, nil
}

func samePCIAddress(a, b string) bool {
	left, ok := parsePCIAddress(a)
	if !ok {
		return false
	}
	right, ok := parsePCIAddress(b)
	if !ok {
		return false
	}
	return left == right
}

// pciAddress is the numeric identity of a PCI bus/device/function. Linux and
// Cloud APIs use both four- and eight-hex-digit domain forms, for example
// 0000:17:00.0 and 00000000:17:00.0. Comparing the parsed components avoids
// treating those equivalent representations as different devices.
type pciAddress struct {
	domain   uint64
	bus      uint64
	device   uint64
	function uint64
}

func parsePCIAddress(raw string) (pciAddress, bool) {
	parts := strings.Split(strings.ToLower(strings.TrimSpace(raw)), ":")
	if len(parts) != 2 && len(parts) != 3 {
		return pciAddress{}, false
	}

	domainPart := "0"
	busPart := parts[0]
	deviceFunctionPart := parts[1]
	if len(parts) == 3 {
		domainPart = parts[0]
		busPart = parts[1]
		deviceFunctionPart = parts[2]
	}
	deviceFunction := strings.Split(deviceFunctionPart, ".")
	if len(deviceFunction) != 2 {
		return pciAddress{}, false
	}

	domain, ok := parsePCIHex(domainPart, 8)
	if !ok {
		return pciAddress{}, false
	}
	bus, ok := parsePCIHex(busPart, 2)
	if !ok {
		return pciAddress{}, false
	}
	device, ok := parsePCIHex(deviceFunction[0], 2)
	if !ok || device > 0x1f {
		return pciAddress{}, false
	}
	function, ok := parsePCIHex(deviceFunction[1], 1)
	if !ok || function > 0x7 {
		return pciAddress{}, false
	}
	return pciAddress{domain: domain, bus: bus, device: device, function: function}, true
}

func parsePCIHex(value string, maxDigits int) (uint64, bool) {
	value = strings.TrimSpace(value)
	if value == "" || len(value) > maxDigits {
		return 0, false
	}
	parsed, err := strconv.ParseUint(value, 16, 64)
	return parsed, err == nil
}

// ReserveCloudResources records ownership before the provider write. A rejected
// initial request removes its pending record; uncertain requests remain protected
// and can be resumed by the same operation.
func (s *Service) ReserveCloudResources(ctx context.Context, nodeUUID, instanceUUID, operationID string, mutations []ResourceMutation) ([]cloudstore.CloudResourceLease, error) {
	s.resourceMu.Lock()
	defer s.resourceMu.Unlock()
	if strings.TrimSpace(nodeUUID) == "" || strings.TrimSpace(instanceUUID) == "" || strings.TrimSpace(operationID) == "" || len(mutations) == 0 {
		return nil, fmt.Errorf("invalid Cloud resource reservation")
	}
	if err := ctx.Err(); err != nil {
		return nil, err
	}
	host, err := s.cloudStore.CloudHostByNodeUUID(nodeUUID)
	if err != nil {
		return nil, err
	}
	gpuIDs := make([]string, len(mutations))
	seen := make(map[string]bool, len(mutations))
	for index, mutation := range mutations {
		if err := validateGPUReservation(mutation); err != nil {
			return nil, err
		}
		if seen[mutation.ResourceID] {
			return nil, fmt.Errorf("duplicate Cloud GPU reservation")
		}
		seen[mutation.ResourceID] = true
		gpuIDs[index] = mutation.ResourceID
	}
	resource, err := s.currentCloudHost(ctx, host, gpuIDs)
	if err != nil {
		return nil, err
	}
	ownership, err := s.gpuOwnershipDescription(nodeUUID)
	if err != nil {
		return nil, err
	}
	for index, mutation := range mutations {
		if _, err := s.gpuReservationShared(host, resource, mutation.ResourceID, instanceUUID, operationID+"/"+fmt.Sprint(index)); err != nil {
			return nil, err
		}
	}
	created := make([]cloudstore.CloudResourceLease, 0, len(mutations))
	var acquired []cloudstore.CloudResourceLease
	compensate := func(cause error) ([]cloudstore.CloudResourceLease, error) {
		for _, prior := range acquired {
			if releaseErr := s.releaseCloudResourceLease(ctx, prior.ID); releaseErr != nil {
				cause = fmt.Errorf("%w (resource compensation: %v)", cause, releaseErr)
			}
		}
		return created, cause
	}
	for index, mutation := range mutations {
		operation := operationID + "/" + fmt.Sprint(index)
		existing, lookupErr := s.cloudStore.CloudResourceLeaseByOperation(operation)
		if lookupErr == nil {
			if !leaseMatchesMutation(existing, host, nodeUUID, instanceUUID, mutation) {
				return compensate(cloudstore.ErrCloudConflict)
			}
			if existing.State == cloudstore.CloudLeaseReserved {
				created = append(created, *existing)
				continue
			}
			if !existing.IsReservationPending() {
				return compensate(fmt.Errorf("Cloud resource operation %s cannot reserve in state %s", operation, existing.State))
			}
		}
		if lookupErr != nil && lookupErr != cloudstore.ErrNotFound {
			return compensate(lookupErr)
		}
		shared, err := s.gpuReservationShared(host, resource, mutation.ResourceID, instanceUUID, operation)
		if err != nil {
			return compensate(err)
		}
		for _, gpu := range resource.GPUs {
			if gpu.ExternalID == mutation.ResourceID {
				description, err := addGPUOwnershipDescription(gpu.Description, ownership)
				if err != nil {
					return compensate(err)
				}
				mutation.Description = &description
				shared = shared && hasGPUOwnershipDescription(gpu.Description, ownership)
				break
			}
		}
		state := cloudstore.CloudLeaseReservePending
		if shared {
			state = cloudstore.CloudLeaseReserved
		}
		lease, err := s.cloudStore.CreateCloudResourceLease(&cloudstore.CloudResourceLease{
			OperationID: operation, ProviderID: host.ProviderID, CloudHostID: host.ID,
			NodeUUID: nodeUUID, InstanceUUID: instanceUUID, ResourceType: strings.ToLower(strings.TrimSpace(mutation.ResourceType)), ResourceID: mutation.ResourceID,
			Amount: 1, Unit: "device", State: state,
		})
		if err != nil {
			return compensate(err)
		}
		if shared {
			if lease.IsReservationPending() {
				if err := s.cloudStore.SetCloudResourceLeaseState(lease.ID, cloudstore.CloudLeaseReserved, ""); err != nil {
					return compensate(err)
				}
				lease.State = cloudstore.CloudLeaseReserved
				lease.ErrorCode = ""
			}
			created = append(created, *lease)
			acquired = append(acquired, *lease)
			continue
		}
		if err := s.applyResourceMutation(ctx, host.ProviderID, mutation); err != nil {
			var apiErr *apiclient.APIError
			if lookupErr == cloudstore.ErrNotFound && errors.As(err, &apiErr) && !apiErr.OutcomeUnknown {
				if deleteErr := s.cloudStore.DeletePendingCloudResourceLease(lease.ID); deleteErr != nil {
					err = errors.Join(err, fmt.Errorf("remove rejected Cloud reservation: %w", deleteErr))
				}
			} else {
				// A rejected retry does not settle an earlier uncertain request.
				if stateErr := s.cloudStore.SetCloudResourceLeaseState(lease.ID, cloudstore.CloudLeaseReservePending, "provider_write_unknown"); stateErr != nil {
					err = errors.Join(err, stateErr)
				}
			}
			return compensate(err)
		}
		if err := s.cloudStore.SetCloudResourceLeaseState(lease.ID, cloudstore.CloudLeaseReserved, ""); err != nil {
			return compensate(err)
		}
		lease.State = cloudstore.CloudLeaseReserved
		lease.ErrorCode = ""
		created = append(created, *lease)
		acquired = append(acquired, *lease)
	}
	return created, nil
}

func leaseMatchesMutation(lease *cloudstore.CloudResourceLease, host *cloudstore.CloudHost, nodeUUID, instanceUUID string, mutation ResourceMutation) bool {
	if lease == nil || host == nil {
		return false
	}
	return lease.ProviderID == host.ProviderID && lease.CloudHostID == host.ID &&
		lease.NodeUUID == nodeUUID && lease.InstanceUUID == instanceUUID &&
		strings.EqualFold(strings.TrimSpace(lease.ResourceType), strings.TrimSpace(mutation.ResourceType)) &&
		lease.ResourceID == mutation.ResourceID && lease.Amount == 1 &&
		lease.Unit == "device"
}

// ReleaseCloudResourceLease only removes GPUWorks' own lease. The GPU is
// re-enabled only when this ledger proves that this operation disabled it.
func (s *Service) ReleaseCloudResourceLease(ctx context.Context, leaseID uint) error {
	s.resourceMu.Lock()
	defer s.resourceMu.Unlock()
	return s.releaseCloudResourceLease(ctx, leaseID)
}

func (s *Service) releaseCloudResourceLease(ctx context.Context, leaseID uint) error {
	lease, err := s.cloudStore.CloudResourceLeaseByID(leaseID)
	if err != nil {
		return err
	}
	if lease.State == cloudstore.CloudLeaseReleased {
		return nil
	}
	if lease.State != cloudstore.CloudLeaseReserved &&
		lease.State != cloudstore.CloudLeaseReleasePending {
		return fmt.Errorf("Cloud resource lease %d has unknown release outcome", lease.ID)
	}
	if lease.ResourceType != "gpu" {
		return provider.ErrUnsupported
	}
	holders, err := s.cloudStore.ActiveCloudResourceLeasesForResource(lease.ProviderID, lease.ResourceType, lease.ResourceID)
	if err != nil {
		return err
	}
	for _, holder := range holders {
		if holder.ID != lease.ID {
			return s.cloudStore.SetCloudResourceLeaseState(lease.ID, cloudstore.CloudLeaseReleased, "")
		}
	}
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := s.cloudStore.SetCloudResourceLeaseState(lease.ID, cloudstore.CloudLeaseReleasePending, ""); err != nil {
		return err
	}
	ownership, err := s.gpuOwnershipDescription(lease.NodeUUID)
	if err != nil {
		return err
	}
	driver, creds, err := s.driverCreds(lease.ProviderID)
	if err != nil {
		return err
	}
	inspector, ok := driver.(ResourceInspector)
	if !ok {
		return provider.ErrUnsupported
	}
	mutation := ResourceMutation{ResourceType: lease.ResourceType, ResourceID: lease.ResourceID, DesiredState: "Enabled"}
	observation, err := inspector.InspectResource(ctx, creds, mutation)
	if err != nil {
		return err
	}
	description := removeGPUOwnershipDescription(observation.Description, ownership)
	mutation.Description = &description
	if err := s.applyResourceMutation(ctx, lease.ProviderID, mutation); err != nil {
		_ = s.cloudStore.SetCloudResourceLeaseState(lease.ID, cloudstore.CloudLeaseReleasePending, "release_unknown")
		return err
	}
	return s.cloudStore.SetCloudResourceLeaseState(lease.ID, cloudstore.CloudLeaseReleased, "")
}

// CPU and memory lease accounting is deferred; deployments reserve assigned GPUs.
func validateGPUReservation(m ResourceMutation) error {
	if strings.TrimSpace(m.ResourceID) == "" {
		return fmt.Errorf("Cloud resource ID is required")
	}
	if !strings.EqualFold(strings.TrimSpace(m.ResourceType), "gpu") {
		return provider.ErrUnsupported
	}
	if m.DesiredState != "Disabled" {
		return fmt.Errorf("GPU reservation must disable the device")
	}
	return nil
}
