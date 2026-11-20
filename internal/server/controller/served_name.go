package controller

import (
	"gpuworks.ai/gpuworks/internal/server/planning"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// nodeCaps is what node's agent announced, as the hub knows it; nil when it
// is not connected.
func (c *Controller) nodeCaps(node *store.Node) []string {
	if node == nil {
		return nil
	}
	if hub, ok := c.hub.(interface{ NodeCaps(string) []string }); ok {
		return hub.NodeCaps(node.UUID)
	}
	return nil
}

// translatesServedName is planning.TranslatesServedName for node's agent, as
// the hub knows it.
func (c *Controller) translatesServedName(node *store.Node) bool {
	if node == nil {
		return false
	}
	return planning.TranslatesServedName(node.RuntimeDriver, c.nodeCaps(node))
}
