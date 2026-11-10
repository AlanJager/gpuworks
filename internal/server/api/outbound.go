package api

import (
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/outbound"
	"gpuworks.ai/gpuworks/internal/server/planning"
)

// OutboundPolicySettings is the server's optional live machine policy. A
// present source is authoritative even when it returns no private exceptions.
// Static in-process callers can continue using Outside.AllowPrivateHosts.
type OutboundPolicySettings interface {
	OutboundAllowPrivateHosts() []string
}

func (a *apiHandlers) outboundPrivateHosts() []string {
	if settings, ok := a.d.Server.(OutboundPolicySettings); ok {
		return settings.OutboundAllowPrivateHosts()
	}
	return a.d.Outside.AllowPrivateHosts
}

// Callers close idle connections after their bounded operation completes.
func (a *apiHandlers) outboundTransport() *outbound.SafeTransport {
	return outbound.TransportWithPrivateHosts(func() (proto.NetworkConfig, error) { return planning.ControlPlaneNetwork(a.d.Store) }, a.outboundPrivateHosts)
}
