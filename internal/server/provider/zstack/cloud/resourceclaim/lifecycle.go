package resourceclaim

import (
	"context"

	"gpuworks.ai/gpuworks/internal/server/provider"
)

// BeforeCreate is called through provider.Service before the controller persists
// or starts an instance. It reserves the selected Cloud GPUs.
func (s *Service) BeforeCreate(ctx context.Context, req provider.InferenceNodeCreateRequest) error {
	mutations, err := s.CloudResourceMutations(req.NodeUUID, req.GPUIndexes)
	if err != nil {
		return err
	}
	if len(mutations) == 0 {
		return nil
	}
	_, err = s.ReserveCloudResources(ctx, req.NodeUUID, req.InstanceUUID, req.InstanceUUID, mutations)
	return err
}

// OnCreateFailed compensates a successful reservation when the controller
// cannot persist the new instances.
func (s *Service) OnCreateFailed(ctx context.Context, req provider.InferenceNodeCreateRequest) error {
	return s.releaseInstanceLeases(ctx, req.InstanceUUID)
}

// RequiresStopConfirmation tells the controller to wait for a terminal agent
// report before releasing resources owned by a Cloud node.
func (s *Service) RequiresStopConfirmation(_ context.Context, nodeUUID string) (bool, error) {
	return s.IsCloudNode(nodeUUID)
}

// AfterInstanceTerminal releases the instance leases during controller
// finalization, before its instance record can be deleted.
func (s *Service) AfterInstanceTerminal(ctx context.Context, req provider.InferenceNodeStopRequest) error {
	return s.releaseInstanceLeases(ctx, req.InstanceUUID)
}
