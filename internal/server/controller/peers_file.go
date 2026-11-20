package controller

import (
	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// peersFor is the peers a rank on node launches with: the group's, when the
// node's agent writes a peers file (planning.PeersFileSupported, as the hub
// knows its capabilities), and none otherwise, so the launch names no file
// the agent would never write.
func (c *Controller) peersFor(node *store.Node, peers []proto.PeerRank) []proto.PeerRank {
	if node == nil || len(peers) == 0 {
		return nil
	}
	var caps []string
	if hub, ok := c.hub.(interface{ NodeCaps(string) []string }); ok {
		caps = hub.NodeCaps(node.UUID)
	}
	if !planning.PeersFileSupported(caps) {
		return nil
	}
	return peers
}
