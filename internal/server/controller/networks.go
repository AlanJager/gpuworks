package controller

import (
	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/network"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// Networks answers the controller's network questions; the app wires
// internal/server/network's Service in.
type Networks interface {
	// ComputeGroup picks the network a multi-node group shares.
	ComputeGroup(nodes []store.Node) (netplan.Group, error)
	// StoragePath picks the network a direct copy between two nodes uses.
	StoragePath(sourceID, targetID uint) (netplan.StoragePath, error)
	// CanServe reports whether a node may host an inference service.
	CanServe(node store.Node) bool
	// NodeNetworks resolves one node.
	NodeNetworks(nodeID uint) (netplan.Resolution, error)
	// Resolve resolves many nodes in one pass, for a whole candidate pool.
	Resolve(nodes []store.Node) (netplan.Resolution, error)
	// ServingEndpoints lists the nodes serving over a network of their own.
	ServingEndpoints() (map[string]network.ServingEndpoint, error)
	// Endpoints is what one network resolves to on each node, for its tests.
	Endpoints(networkID uint, nodes []store.Node) (netplan.Resolution, error)
}

// SetNetworks replaces the network service. The app passes the one every
// component shares, so they all read the same catalog.
func (c *Controller) SetNetworks(n Networks) { c.networks = n }
