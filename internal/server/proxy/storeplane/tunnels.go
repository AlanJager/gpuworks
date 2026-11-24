package storeplane

import (
	"context"
	"net"
)

// AgentTunnels is what the hub offers the gateway.
type AgentTunnels interface {
	OpenTunnel(ctx context.Context, nodeUUID, dst string) (net.Conn, error)
	Online(nodeUUID string) bool
}

// Tunnels opens the gateway's tunnels through the hub. Errors pass through
// untouched, at open and on an open stream alike: the hub reports a dead data
// lane with tunnelerr.ErrDataLaneUnavailable, the value the gateway knows as
// plane.ErrDataLaneUnavailable.
func Tunnels(h AgentTunnels) *HubTunnels {
	return &HubTunnels{hub: h}
}

// HubTunnels is the hub as the gateway's tunnel provider.
type HubTunnels struct {
	hub AgentTunnels
}

// OpenTunnel opens a connection to dst on the node's agent.
func (t *HubTunnels) OpenTunnel(ctx context.Context, nodeUUID, dst string) (net.Conn, error) {
	return t.hub.OpenTunnel(ctx, nodeUUID, dst)
}

// Online reports whether the node's agent is connected.
func (t *HubTunnels) Online(nodeUUID string) bool { return t.hub.Online(nodeUUID) }
