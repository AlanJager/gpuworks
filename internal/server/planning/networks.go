package planning

import (
	"gpuworks.ai/gpuworks/internal/server/deploy"
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Networks answers the network questions a launch plan has; the server wires
// internal/server/network's Service in.
type Networks interface {
	// CanServe reports whether the node may host an inference service.
	CanServe(node store.Node) bool
}

// nodeResolverOf is the network resolution networks offers, when it offers
// one: the network service does, a test double may not.
func nodeResolverOf(networks Networks) NodeResolver {
	if resolver, ok := networks.(interface {
		Resolve([]store.Node) (netplan.Resolution, error)
	}); ok {
		return resolver.Resolve
	}
	return nil
}

// rankNetwork is what a rank's topology records about the network its group
// shares: the network, and how the rank is launched on it (the interface and
// RDMA ports its collectives use). The preview and the real launch both take
// it from here.
func rankNetwork(compute netplan.Resolved) (uint, string, deploy.ComputeNetwork) {
	return compute.Network.ID, compute.Network.Name, deploy.ComputeNetwork{
		Type: compute.Network.Type, Interface: compute.Interface, HCAs: compute.HCAs, LinkLayer: compute.LinkLayer,
	}
}
