package proxy

import (
	"gpuworks.ai/gpuworks/internal/server/proxy/plane"
	"gpuworks.ai/gpuworks/internal/server/proxy/storeplane"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// storeOf returns the store behind a gateway's source, for the tests that
// seed and inspect rows directly. Production code has no such way back: the
// architecture check keeps the adapter package out of the gateway.
func storeOf(src plane.Source) *store.Store {
	if s, ok := src.(*storeplane.Source); ok && s != nil {
		return s.Store
	}
	return nil
}
