package api

import (
	"gpuworks.ai/gpuworks/internal/server/api/dto"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// toNodeDTO is n's API view with no scheduling holder registered, as the
// wire tests that predate holders build it.
func toNodeDTO(n *store.Node) dto.Node { return nodeDTO(n, false, store.NodeHold{}) }
