package controller

import (
	"slices"

	"gpuworks.ai/gpuworks/internal/proto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// translatesServedName reports whether node's agent would translate the
// value of --served-model-name to a host path: a host-process agent that does
// not advertise proto.CapLaunchNameVerbatim. Its launches with no alias are
// left to the engine's default name (deploy.Params.ImplicitServedName); an
// agent that is offline counts as old, since what it supports is unknown.
// Container agents run the name inside the container, where nothing is
// translated, whatever their version.
func (c *Controller) translatesServedName(node *store.Node) bool {
	if node == nil || node.RuntimeDriver != proto.RuntimeDriverHost {
		return false
	}
	caps, ok := c.hub.(interface{ NodeCaps(string) []string })
	if !ok {
		return true
	}
	return !slices.Contains(caps.NodeCaps(node.UUID), proto.CapLaunchNameVerbatim)
}
