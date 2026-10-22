package api

import (
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/outbound"
	"gpuworks.ai/gpuworks/internal/server/planning"
)

// Callers close idle connections after their bounded operation completes.
func (a *apiHandlers) outboundTransport() *outbound.SafeTransport {
	return outbound.Transport(func() (proto.NetworkConfig, error) { return planning.ControlPlaneNetwork(a.d.Store) }, a.d.Outside.AllowPrivateHosts...)
}
