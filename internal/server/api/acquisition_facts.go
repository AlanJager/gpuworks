package api

import "gpuworks.ai/gpuworks/internal/server/store"

// nodeDisplayName is store.Node.DisplayName for a possibly absent node.
func nodeDisplayName(node *store.Node) string {
	if node == nil {
		return ""
	}
	return node.DisplayName()
}
