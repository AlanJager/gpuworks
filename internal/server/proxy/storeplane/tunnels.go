package storeplane

import (
	"context"
	"errors"
	"net"

	"gpuworks.ai/gpuworks/internal/server/hub"
	"gpuworks.ai/gpuworks/internal/server/proxy/plane"
)

// AgentTunnels is what the hub offers the gateway.
type AgentTunnels interface {
	OpenTunnel(ctx context.Context, nodeUUID, dst string) (net.Conn, error)
	Online(nodeUUID string) bool
}

// Tunnels opens the gateway's tunnels through the hub. A detached data lane
// is reported as the gateway knows it, plane.ErrDataLaneUnavailable, and still
// as the hub's own error, with the hub's text.
func Tunnels(h AgentTunnels) *HubTunnels {
	return &HubTunnels{hub: h}
}

// HubTunnels is the hub as the gateway's tunnel provider.
type HubTunnels struct {
	hub AgentTunnels
}

// OpenTunnel opens a connection to dst on the node's agent.
func (t *HubTunnels) OpenTunnel(ctx context.Context, nodeUUID, dst string) (net.Conn, error) {
	conn, err := t.hub.OpenTunnel(ctx, nodeUUID, dst)
	if err != nil && errors.Is(err, hub.ErrDataLaneUnavailable) {
		return nil, laneUnavailable{err}
	}
	return conn, err
}

// Online reports whether the node's agent is connected.
func (t *HubTunnels) Online(nodeUUID string) bool { return t.hub.Online(nodeUUID) }

type laneUnavailable struct{ error }

func (e laneUnavailable) Unwrap() []error {
	return []error{e.error, plane.ErrDataLaneUnavailable}
}
