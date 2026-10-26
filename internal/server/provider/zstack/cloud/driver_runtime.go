package cloud

import (
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/provider"
	"gpuworks.ai/gpuworks/internal/server/provider/zstack/cloud/resourceclaim"
	"gpuworks.ai/gpuworks/internal/server/provision"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// NewDriver owns the assembly of Cloud's installation and resource services.
// The registry only holds a prototype; application state belongs to this copy.
func (d *Driver) NewDriver(st *store.Store, providers *provider.Service, installer provision.Installer) provider.Driver {
	return &Driver{
		store: st, installer: installer,
		resourceClaims: resourceclaim.New(st, providers),
		clientOptions:  d.clientOptions,
		diagnostics:    d.diagnostics,
	}
}

func (d *Driver) InferenceNodeLifecycleFor(nodeUUID string) (provider.InferenceNodeLifecycleHandler, error) {
	if d.resourceClaims == nil {
		return nil, fmt.Errorf("Cloud resource service is not configured")
	}
	managed, err := d.resourceClaims.IsCloudNode(nodeUUID)
	if err != nil || !managed {
		return nil, err
	}
	return d.resourceClaims, nil
}

var (
	_ provider.DriverFactory                  = (*Driver)(nil)
	_ provider.InferenceNodeLifecycleProvider = (*Driver)(nil)
	_ resourceclaim.HostResourceQuerier       = (*Driver)(nil)
	_ resourceclaim.ResourceMutator           = (*Driver)(nil)
	_ resourceclaim.ResourceInspector         = (*Driver)(nil)
)
