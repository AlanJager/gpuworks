//go:build linux && dqlite

package app

import (
	"testing"

	"gpuworks.ai/gpuworks/internal/server/bus"
	"gpuworks.ai/gpuworks/internal/server/store"
)

// openServerDatabase opens the database a test prepares before New in the
// form this build's server owns: the single-member replicated database here.
// A SQLite file at the same path would be refused by New.
func openServerDatabase(t *testing.T, path string) (*store.Store, error) {
	t.Helper()
	return store.OpenRuntime(t.Context(), path, bus.New(), store.ReplicatedBackend())
}
