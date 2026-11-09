package hub

import (
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/outbound"
	"gpuworks.ai/gpuworks/internal/server/planning"
)

// SetOutboundAllowPrivateHosts sets static administrator exceptions before Run.
func (h *Hub) SetOutboundAllowPrivateHosts(hosts []string) {
	h.outboundAllowPrivateHosts = append([]string(nil), hosts...)
	h.readPrivateHosts = nil
}

// SetOutboundPrivateHostsReader installs the shared, concurrency-safe machine
// policy before Run. Its results are read again on every outbound request.
func (h *Hub) SetOutboundPrivateHostsReader(read func() []string) {
	h.readPrivateHosts = read
}

func (h *Hub) outboundPrivateHosts() []string {
	if h.readPrivateHosts != nil {
		return h.readPrivateHosts()
	}
	return h.outboundAllowPrivateHosts
}

func (h *Hub) outboundTransport() *outbound.SafeTransport {
	return outbound.TransportWithPrivateHosts(func() (proto.NetworkConfig, error) { return planning.ControlPlaneNetwork(h.store) }, h.outboundPrivateHosts)
}
