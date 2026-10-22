package resourceclaim

import (
	"context"

	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/inventory"
)

// HostResourceQuerier reads the selected host and GPUs for reservation. Each
// query returns all requested resources or an error, never partial success.
type HostResourceQuerier interface {
	QueryHostResources(context.Context, provider.Credentials, string, []string) (inventory.CloudResource, error)
}

// ResourceMutation describes a GPUWorks-owned Cloud resource change.
// CPU and memory use a signed delta; GPU uses a desired Cloud state.
type ResourceMutation struct {
	ResourceType string
	ResourceID   string
	Delta        int64
	DesiredState string
	Description  *string
}

// ResourceMutator applies an ownership change through the Cloud driver.
type ResourceMutator interface {
	ApplyResourceMutation(context.Context, provider.Credentials, ResourceMutation) error
}

// ResourceInspector reads the post-write state needed to classify a provider
// call whose result was interrupted. It must not mutate provider state.
type ResourceInspector interface {
	InspectResource(context.Context, provider.Credentials, ResourceMutation) (ResourceObservation, error)
}

// ResourceObservation is the live state used to reconcile an interrupted write.
type ResourceObservation struct {
	ResourceType string
	ResourceID   string
	Amount       int64
	State        string
	Description  string
}
