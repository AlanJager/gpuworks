package controller

import (
	"context"
	"fmt"

	"gpuworks.ai/gpuworks/internal/server/netplan"
	"gpuworks.ai/gpuworks/internal/server/servingdial"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// checkServingPath runs after an instance is scheduled and before its node is
// told to start it. A node on a serving network is reached over that network
// only, so the network must answer now, in the direction serving traffic
// flows; a node without one serves over the agent tunnel if its management
// network may carry serving at all.
func (c *Controller) checkServingPath(ctx context.Context, node *store.Node) error {
	networks, err := c.networks.NodeNetworks(node.ID)
	if err != nil {
		return err
	}
	resolved, ok := networks.For(node.ID, netplan.RoleServing)
	if !ok || resolved.IsManagement() {
		// The agent tunnel needs no address, only the role.
		if !c.networks.CanServe(*node) {
			return fmt.Errorf("node %s has no network that may carry serving traffic: it is on no serving network and its management network may not carry serving", node.DisplayName())
		}
		return nil
	}
	if resolved.IP == "" {
		return fmt.Errorf("serving network %s has no address on node %s", resolved.Network.Name, node.DisplayName())
	}
	endpoints, err := c.networks.ServingEndpoints()
	if err != nil {
		return err
	}
	ep, ok := endpoints[node.UUID]
	if !ok {
		return fmt.Errorf("node %s has no token to reach its serving forwarder", node.DisplayName())
	}
	dial := c.servingDial
	if dial == nil {
		dial = servingdial.Dialer()
	}
	if err := servingdial.Probe(ctx, dial, ep.IP, ep.NodeToken); err != nil {
		return fmt.Errorf("serving network %s: the server cannot reach %s: %w", resolved.Network.Name, ep.IP, err)
	}
	return nil
}
