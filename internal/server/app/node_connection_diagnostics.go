package app

import (
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/expose"
	"gpuworks.ai/gpuworks/internal/server/provision"
)

// The wrapper preserves all installer capabilities and adds only a read of
// the connector's historical observation, with no new API dependency field.
type connectionDiagnosticInstaller struct {
	*provision.SSHInstaller
	exposure *expose.Store
}

func connectionInstaller(installer *provision.SSHInstaller, dataDir string) *connectionDiagnosticInstaller {
	return &connectionDiagnosticInstaller{SSHInstaller: installer, exposure: expose.NewStore(dataDir)}
}

func (i *connectionDiagnosticInstaller) ConnectionExposure() *dto.NodeConnectionExposure {
	status, err := i.exposure.ReadStatus()
	if err != nil || status == nil || status.UpdatedAt.IsZero() || provision.ConnectionDiagnosticURL(status.URL) == "" {
		return nil
	}
	state := status.State
	switch state {
	case expose.StateReady, expose.StateStarting, expose.StateAwaitingRoute, expose.StateDegraded, expose.StateDisabled, expose.StateFailed:
	default:
		state = "unknown"
	}
	return &dto.NodeConnectionExposure{URL: status.URL, State: state, ObservedAt: status.UpdatedAt}
}
